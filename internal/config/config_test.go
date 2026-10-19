package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRetiredGmailQuotaConfigurationIsRejected(t *testing.T) {
	for _, value := range []int{0, 4800, -1} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := fmt.Sprintf(`{"gmail":{"quota_units_per_minute":%d}}`, value)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), `unknown field "quota_units_per_minute"`) {
				t.Fatalf("retired Gmail quota field error = %v", err)
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
		config.RoleLabels != nil || !config.Delete {
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

func TestLegacyRoleLabelFieldsAreRejected(t *testing.T) {
	for _, field := range []string{"trash_label", "spam_label"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(`{%q:"Legacy"}`, field)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), field) {
				t.Fatalf("legacy field error = %v", err)
			}
		})
	}
}

func TestRoleLabelConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       map[string]string
	}{
		{name: "omitted", data: `{}`},
		{name: "overrides", data: `{"role_labels":{"archive":"Fastmail_Archive","trash":"X","junk":"Y"}}`, want: map[string]string{"archive": "Fastmail_Archive", "trash": "X", "junk": "Y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.RoleLabels, tc.want) {
				t.Fatalf("role labels=%v, want %v", got.RoleLabels, tc.want)
			}
		})
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
