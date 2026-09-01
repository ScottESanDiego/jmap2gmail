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

func AuthenticatedClient(ctx context.Context, credentialsPath, tokenPath string) (*http.Client, error) {
	config, err := OAuthConfig(credentialsPath)
	if err != nil {
		return nil, err
	}
	token, err := loadToken(tokenPath)
	if err != nil {
		return nil, err
	}
	return config.Client(ctx, token), nil
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
	fmt.Fprintf(output, "Open this URL in a browser to authorize jmapsync:\n\n%s\n\n", authURL)

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
		_, _ = io.WriteString(writer, "jmapsync authorization complete. You may close this window.\n")
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
		return errors.New("Google returned no refresh token; revoke the app grant and authorize again")
	}
	if err := saveToken(tokenPath, token); err != nil {
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
