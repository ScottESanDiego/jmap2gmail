package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "fastmail": {"token_file": "fastmail-token"},
  "gmail": {"credentials_file": "google.json", "token_file": "token.json"},
  "delete": true
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Fastmail.TokenFile != filepath.Join(dir, "fastmail-token") ||
		config.Gmail.CredentialsFile != filepath.Join(dir, "google.json") ||
		config.TrashLabel != "Fastmail_Trash" || !config.Delete {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestUnknownConfigFieldFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"typo": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}
