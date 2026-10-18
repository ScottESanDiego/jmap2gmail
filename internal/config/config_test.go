package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGmailQuotaConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        int
		invalid     bool
	}{
		{name: "default uses adapter rate"},
		{name: "zero uses adapter rate", field: `,"quota_units_per_minute":0`},
		{name: "positive", field: `,"quota_units_per_minute":2400`, want: 2400},
		{name: "negative", field: `,"quota_units_per_minute":-1`, want: -1, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := fmt.Sprintf(`{"fastmail":{"token_file":"fastmail-token"},"gmail":{"credentials_file":"client.json","token_file":"token.json"%s}}`, tc.field)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Gmail.QuotaUnitsPerMinute != tc.want {
				t.Fatalf("quota=%d, want %d", got.Gmail.QuotaUnitsPerMinute, tc.want)
			}
			err = got.ValidateForSync()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "gmail.quota_units_per_minute") {
					t.Fatalf("negative quota error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := got.ValidateForAuth(); err != nil {
				t.Fatalf("quota affected authorization: %v", err)
			}
		})
	}
}

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

func TestLoadIncludeMailboxes(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{name: "omitted"},
		{name: "empty", field: `,"include_mailboxes":[]`, want: []string{}},
		{name: "paths", field: `,"include_mailboxes":["Inbox","Projects/Active"]`, want: []string{"Inbox", "Projects/Active"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"fastmail":{"token_file":"token"`+tc.field+`}}`), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Fastmail.IncludeMailboxes, tc.want) {
				t.Fatalf("include_mailboxes=%#v want %#v", got.Fastmail.IncludeMailboxes, tc.want)
			}
		})
	}
}
