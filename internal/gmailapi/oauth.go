package gmailapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sys/unix"
	"google.golang.org/api/gmail/v1"
)

func OAuthConfig(credentialsPath string) (*oauth2.Config, error) {
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read Google OAuth credentials: %w", err)
	}
	config, err := google.ConfigFromJSON(data, gmail.GmailModifyScope)
	if err != nil {
		return nil, fmt.Errorf("parse Google OAuth credentials: %w", err)
	}
	return config, nil
}

// AuthenticatedClient refreshes credentials before sync work begins. Persistent
// renewal is enabled only for apply runs; dry runs never create token artifacts.
func AuthenticatedClient(ctx context.Context, credentialsPath, tokenPath string, persist bool) (*http.Client, error) {
	config, err := OAuthConfig(credentialsPath)
	if err != nil {
		return nil, err
	}
	var source oauth2.TokenSource
	if persist {
		source = &persistentTokenSource{ctx: ctx, config: config, path: tokenPath}
	} else {
		token, err := loadToken(tokenPath)
		if err != nil {
			return nil, err
		}
		source = config.TokenSource(ctx, token)
	}
	if _, err := source.Token(); err != nil {
		if !persist {
			err = tokenRenewalError(err)
		}
		return nil, err
	}
	client := oauth2.NewClient(ctx, source)
	client.Timeout = 60 * time.Second
	return client, nil
}

// Each call reloads under the stable sidecar lock, so separate processes see
// newly issued credentials. Persist before returning a renewed token to HTTP.
type persistentTokenSource struct {
	ctx    context.Context
	config *oauth2.Config
	path   string
}

func (s *persistentTokenSource) Token() (*oauth2.Token, error) {
	var token *oauth2.Token
	err := withTokenLock(s.ctx, s.path, func() error {
		current, err := loadToken(s.path)
		if err != nil {
			return err
		}
		if current.Valid() {
			token = current
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		renewed, err := s.config.TokenSource(refreshCtx, current).Token()
		if err != nil {
			return tokenRenewalError(err)
		}
		// Never discard the existing refresh token when Google omits it.
		if renewed.RefreshToken == "" {
			renewed.RefreshToken = current.RefreshToken
		}
		if tokenChanged(current, renewed) {
			if err := saveToken(s.path, renewed); err != nil {
				return err
			}
		}
		token = renewed
		return nil
	})
	return token, err
}

func tokenChanged(a, b *oauth2.Token) bool {
	return a.AccessToken != b.AccessToken || a.RefreshToken != b.RefreshToken ||
		a.TokenType != b.TokenType || !a.Expiry.Equal(b.Expiry)
}

func tokenRenewalError(err error) error {
	if response, ok := errors.AsType[*oauth2.RetrieveError](err); ok && response.ErrorCode == "invalid_grant" {
		return fmt.Errorf("Google OAuth authorization is no longer valid; run jmapsync google-auth again: %w", err)
	}
	return fmt.Errorf("renew Google OAuth token: %w", err)
}

// The lock covers load, refresh, and atomic replacement. A separate inode is
// essential because renaming the token file would invalidate a lock on it.
func withTokenLock(ctx context.Context, path string, fn func() error) error {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open Google token lock: %w", err)
	}
	defer file.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("wait for Google token lock: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock Google token: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for Google token lock: %w", waitCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return fn()
}

func Authorize(ctx context.Context, credentialsPath, tokenPath string, output io.Writer) error {
	config, err := OAuthConfig(credentialsPath)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start OAuth callback listener: %w", err)
	}
	defer listener.Close()
	config.RedirectURL = "http://" + listener.Addr().String() + "/oauth2/callback"

	state, err := randomState()
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	)
	fmt.Fprintf(output, "Open this URL in a browser and authorize jmapsync to access Gmail:\n\n%s\n\n", authURL)

	type callbackResult struct {
		code string
		err  error
	}
	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/callback", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("state") != state {
			http.Error(writer, "OAuth state mismatch", http.StatusBadRequest)
			result <- callbackResult{err: errors.New("Google OAuth state mismatch")}
			return
		}
		if value := request.URL.Query().Get("error"); value != "" {
			http.Error(writer, "Authorization failed: "+value, http.StatusBadRequest)
			result <- callbackResult{err: fmt.Errorf("Google authorization failed: %s", value)}
			return
		}
		code := request.URL.Query().Get("code")
		if code == "" {
			http.Error(writer, "Missing authorization code", http.StatusBadRequest)
			result <- callbackResult{err: errors.New("Google OAuth callback has no code")}
			return
		}
		_, _ = io.WriteString(writer, "Authorization received. Return to jmapsync to check the result. You can close this window.\n")
		result <- callbackResult{code: code}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	var callback callbackResult
	select {
	case <-ctx.Done():
		_ = server.Close()
		return ctx.Err()
	case err := <-serveDone:
		return fmt.Errorf("OAuth callback server: %w", err)
	case callback = <-result:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if callback.err != nil {
		return callback.err
	}
	token, err := config.Exchange(ctx, callback.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("exchange Google authorization code: %w", err)
	}
	if token.RefreshToken == "" {
		return errors.New("Google returned no refresh token; revoke jmapsync's access in your Google Account, then run jmapsync google-auth again")
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return fmt.Errorf("create Google token directory: %w", err)
	}
	if err := withTokenLock(ctx, tokenPath, func() error { return saveToken(tokenPath, token) }); err != nil {
		return err
	}
	fmt.Fprintf(output, "Saved Google OAuth token to %s\n", tokenPath)
	return nil
}

func randomState() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func loadToken(path string) (*oauth2.Token, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Google OAuth token: %w", err)
	}
	defer file.Close()
	var token oauth2.Token
	if err := json.NewDecoder(file).Decode(&token); err != nil {
		return nil, fmt.Errorf("decode Google OAuth token: %w", err)
	}
	if token.RefreshToken == "" && token.AccessToken == "" {
		return nil, errors.New("Google OAuth token contains no usable token")
	}
	return &token, nil
}

func saveToken(path string, token *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Google token directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".jmapsync-google-token-*")
	if err != nil {
		return fmt.Errorf("create temporary Google token: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect Google token: %w", err)
	}
	if err := json.NewEncoder(tmp).Encode(token); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode Google token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync Google token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Google token: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit Google token: %w", err)
	}
	committed = true
	return nil
}
