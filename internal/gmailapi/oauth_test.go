package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
)

func TestOAuthConfigRequestsOnlyGmailModify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	credentials := `{
  "installed": {
    "client_id": "client-id",
    "client_secret": "client-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}`
	if err := os.WriteFile(path, []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := OAuthConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{gmail.GmailModifyScope}; !reflect.DeepEqual(config.Scopes, want) {
		t.Fatalf("OAuth scopes = %v, want %v", config.Scopes, want)
	}
}

func oauthFixture(t *testing.T, handler http.HandlerFunc, token *oauth2.Token) (string, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	root := t.TempDir()
	credentials := filepath.Join(root, "credentials.json")
	data := fmt.Sprintf(`{"installed":{"client_id":"client-id","client_secret":"secret","auth_uri":"%s/auth","token_uri":"%s/token","redirect_uris":["http://localhost"]}}`, server.URL, server.URL)
	if err := os.WriteFile(credentials, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "token.json")
	if err := saveToken(path, token); err != nil {
		t.Fatal(err)
	}
	return credentials, path
}

func expiredToken() *oauth2.Token {
	return &oauth2.Token{AccessToken: "old", RefreshToken: "refresh-old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}
}

func TestAuthenticatedClientRequestTimeout(t *testing.T) {
	for _, persist := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist=%t", persist), func(t *testing.T) {
			credentials, path := oauthFixture(t, func(http.ResponseWriter, *http.Request) {
				t.Error("valid token should not need renewal")
			}, &oauth2.Token{AccessToken: "valid", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
			client, err := AuthenticatedClient(t.Context(), credentials, path, persist)
			if err != nil {
				t.Fatal(err)
			}
			if client.Timeout != 60*time.Second {
				t.Fatalf("client timeout = %v, want 60s", client.Timeout)
			}
			for _, callerCancels := range []bool{false, true} {
				t.Run(fmt.Sprintf("callerCancels=%t", callerCancels), func(t *testing.T) {
					started := make(chan struct{})
					target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") != "Bearer valid" {
							t.Error("request did not use OAuth transport")
						}
						close(started)
						<-r.Context().Done()
					}))
					defer target.Close()
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					client.Timeout = 100 * time.Millisecond
					if callerCancels {
						client.Timeout = 2 * time.Second
						go func() {
							select {
							case <-started:
								cancel()
							case <-ctx.Done():
							}
						}()
					}
					request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
					if err != nil {
						t.Fatal(err)
					}
					response, err := client.Do(request)
					if response != nil {
						response.Body.Close()
					}
					want := context.DeadlineExceeded
					if callerCancels {
						want = context.Canceled
					}
					if !errors.Is(err, want) {
						t.Fatalf("request error = %v, want %v", err, want)
					}
				})
			}
		})
	}
}

func TestRenewalPersistsAndSerializesClients(t *testing.T) {
	var calls atomic.Int32
	credentials, path := oauthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "refresh-old" {
			t.Errorf("unexpected refresh credential")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"new","refresh_token":"refresh-new","token_type":"Bearer","expires_in":3600}`)
	}, expiredToken())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := AuthenticatedClient(t.Context(), credentials, path, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	token, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "new" || token.RefreshToken != "refresh-new" || !token.Valid() {
		t.Fatalf("renewed token not saved correctly")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token permissions = %o", info.Mode().Perm())
	}
}

func TestDryRunRenewalWritesNothing(t *testing.T) {
	credentials, path := oauthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"new","token_type":"Bearer","expires_in":3600}`)
	}, expiredToken())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticatedClient(t.Context(), credentials, path, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry run changed token")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("dry run created artifacts: %v", entries)
	}
}

func TestRenewalDuringRequestsPreservesRefreshToken(t *testing.T) {
	credentials, path := oauthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"new","token_type":"Bearer","expires_in":3600}`)
	}, &oauth2.Token{AccessToken: "valid", RefreshToken: "refresh-old", Expiry: time.Now().Add(time.Hour)})
	client, err := AuthenticatedClient(t.Context(), credentials, path, true)
	if err != nil {
		t.Fatal(err)
	}
	// Expire the HTTP client's cached token and the on-disk token to simulate
	// a later request in a long sync, rather than only startup renewal.
	transport := client.Transport.(*oauth2.Transport)
	cached, err := transport.Source.Token()
	if err != nil {
		t.Fatal(err)
	}
	cached.Expiry = time.Now().Add(-time.Hour)
	if err := saveToken(path, cached); err != nil {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Error("request did not use renewed token")
		}
		saved, err := loadToken(path)
		if err != nil {
			t.Error(err)
			return
		}
		if saved.AccessToken != "new" || saved.RefreshToken != "refresh-old" {
			t.Error("token not persisted before request")
		}
	}))
	defer target.Close()
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestRevokedGrantFailsWithReauthorizationGuidance(t *testing.T) {
	credentials, path := oauthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}, expiredToken())
	before, _ := os.ReadFile(path)
	_, err := AuthenticatedClient(t.Context(), credentials, path, true)
	if err == nil || !strings.Contains(err.Error(), "google-auth") {
		t.Fatalf("error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed refresh changed token file")
	}
}

func TestTokenLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	err := withTokenLock(t.Context(), path, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := withTokenLock(ctx, path, func() error { t.Error("entered locked section"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock error = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := withTokenLock(t.Context(), path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestTokenChangesIncludeRefreshToken(t *testing.T) {
	original := expiredToken()
	changed := *original
	changed.RefreshToken = "rotated"
	if !tokenChanged(original, &changed) {
		t.Fatal("refresh token rotation ignored")
	}
	if tokenChanged(original, original) {
		t.Fatal("unchanged token reported changed")
	}
}

func TestRenewalSaveFailureReturnsError(t *testing.T) {
	var path string
	credentials, tokenPath := oauthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Make atomic replacement fail after the old credentials were loaded.
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"new","token_type":"Bearer","expires_in":3600}`)
	}, expiredToken())
	path = tokenPath
	client, err := AuthenticatedClient(t.Context(), credentials, path, true)
	if err == nil || client != nil || !strings.Contains(err.Error(), "commit Google token") {
		t.Fatalf("client = %v, error = %v", client, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".jmapsync-google-token-") {
			t.Fatal("failed save left temporary token")
		}
	}
}
