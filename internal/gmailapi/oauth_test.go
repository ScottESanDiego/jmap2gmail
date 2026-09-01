package gmailapi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
