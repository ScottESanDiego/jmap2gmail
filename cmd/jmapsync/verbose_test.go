package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deliberately unsorted, with control characters that must stay on one output line.
const diagnosticMailboxes = `[{"id":"z","name":"Zebra"},{"id":"q","name":"Quoted\"\nFolder"},{"id":"c","name":"Don Trust and Will","parentId":"i"},{"id":"i","name":"Inbox"}]`

func diagnosticConfig(t *testing.T, selectors []string, endpoint string) (string, string) {
	t.Helper()
	root := t.TempDir()
	content := map[string]any{
		"fastmail":   map[string]any{"token_file": "fastmail-token", "session_endpoint": endpoint, "include_mailboxes": selectors},
		"gmail":      map[string]any{"credentials_file": "missing-client.json", "token_file": "google-token.json"},
		"state_file": "state/cache.json", "delete": true,
	}
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, root
}

func diagnosticServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer fastmail-secret-value" {
			t.Error("missing authorization")
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"capabilities":{"urn:ietf:params:jmap:core":{},"urn:ietf:params:jmap:mail":{}},"primaryAccounts":{"urn:ietf:params:jmap:mail":"account-1"},"username":"user@example.com","apiUrl":"http://`+r.Host+`/api","downloadUrl":"http://`+r.Host+`/download/{accountId}/{blobId}/{name}","state":"s1"}`)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(string(data), `"Mailbox/get"`) {
			t.Errorf("unexpected API request: %s", data)
		}
		io.WriteString(w, `{"methodResponses":[["Mailbox/get",{"accountId":"account-1","state":"m1","list":`+diagnosticMailboxes+`},"0"]],"sessionState":"s1"}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestVerboseMissingTokenStartup(t *testing.T) {
	for _, spelling := range []string{"-verbose", "--verbose"} {
		t.Run(spelling, func(t *testing.T) {
			path, root := diagnosticConfig(t, []string{"Inbox"}, "https://example.invalid/session")
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), []string{"sync", spelling, "--dry-run", "--full", "--no-cache", "--config", path}, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "read Fastmail API token") {
				t.Fatalf("error=%v", err)
			}
			for _, want := range []string{"config=" + `"` + path + `"`, "state=" + `"` + filepath.Join(root, "state/cache.json") + `"`, "dry-run=true full=true no-cache=true delete=true", `configured mailbox selectors: ["Inbox"]`} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("missing %q in %s", want, stderr.String())
				}
			}
			if stdout.Len() != 0 || strings.Contains(stderr.String(), "example.invalid") {
				t.Fatalf("unexpected output stdout=%s stderr=%s", &stdout, &stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(err) {
				t.Fatalf("startup created state: %v", err)
			}
		})
	}
}

func TestVerboseMailboxPreflight(t *testing.T) {
	server := diagnosticServer(t)
	for _, test := range []struct {
		name      string
		selectors []string
		verbose   bool
		success   bool
	}{
		{"invalid quiet", []string{"Inbox/Don Trust and Will/"}, false, false},
		{"invalid verbose", []string{"Inbox/Don Trust and Will/"}, true, false},
		{"unknown verbose", []string{"Typo"}, true, false},
		{"selected", []string{"Inbox/Don Trust and Will"}, true, true},
		{"all", nil, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, root := diagnosticConfig(t, test.selectors, server.URL+"/session")
			if err := os.WriteFile(filepath.Join(root, "fastmail-token"), []byte("fastmail-secret-value"), 0600); err != nil {
				t.Fatal(err)
			}
			original := []byte("OAuth-secret-value")
			if err := os.WriteFile(filepath.Join(root, "google-token.json"), original, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"sync", "--config", path}
			if test.verbose {
				args = append(args, "--verbose")
			}
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), args, &stdout, &stderr)
			if err == nil {
				t.Fatal("expected deliberate failure")
			}
			t.Logf("expected failure: %v", err)
			if stdout.Len() != 0 {
				t.Fatalf("diagnostics leaked to stdout: %s", &stdout)
			}
			out := stderr.String()
			if strings.Contains(out, "secret-value") || strings.Contains(out, server.URL) || strings.Contains(out, "methodResponses") {
				t.Fatalf("sensitive output: %s", out)
			}
			if !test.verbose {
				if stderr.Len() != 0 {
					t.Fatalf("quiet stderr: %s", out)
				}
			} else {
				want := "verbose: available Fastmail folders (4):\nverbose:   path=\"Inbox\" id=\"i\"\nverbose:   path=\"Inbox/Don Trust and Will\" id=\"c\"\nverbose:   path=\"Quoted\\\"\\nFolder\" id=\"q\"\nverbose:   path=\"Zebra\" id=\"z\"\n"
				if !strings.Contains(out, want) {
					t.Fatalf("missing sorted escaped folder diagnostics:\n%s\nwant:\n%s", out, want)
				}
			}
			if !test.success {
				if !strings.Contains(err.Error(), "fastmail.include_mailboxes") {
					t.Fatalf("error=%v", err)
				}
				if strings.Contains(out, "selected Fastmail folders") {
					t.Fatalf("invalid selector reported selected: %s", out)
				}
				if test.verbose && strings.HasSuffix(test.selectors[0], "/") {
					for _, hint := range []string{"remove the trailing slash", `("Inbox/Don Trust and Will")`, `("Inbox/Don Trust and Will/*")`} {
						if !strings.Contains(out, hint) {
							t.Errorf("missing hint %q: %s", hint, out)
						}
					}
				}
				for _, artifact := range []string{"state", "google-token.json.lock"} {
					if _, err := os.Stat(filepath.Join(root, artifact)); !os.IsNotExist(err) {
						t.Fatalf("preflight created %s: %v", artifact, err)
					}
				}
			} else {
				if !strings.Contains(err.Error(), "credentials") {
					t.Fatalf("want intentional Gmail credential error: %v", err)
				}
				want := "verbose: selected Fastmail folders (1):\nverbose:   path=\"Inbox/Don Trust and Will\" id=\"c\"\n"
				if len(test.selectors) == 0 {
					want = "verbose: selected Fastmail folders (4) (all mail):"
				}
				if !strings.Contains(out, want) {
					t.Fatalf("missing selected diagnostics %q: %s", want, out)
				}
			}
			if got, err := os.ReadFile(filepath.Join(root, "google-token.json")); err != nil || !bytes.Equal(got, original) {
				t.Fatalf("OAuth token changed: %q %v", got, err)
			}
		})
	}
}

func TestVerboseHasNoShortAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"sync", "-v"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("-v error=%v", err)
	}
}

func TestDiagnosticPathResolvesRelativePaths(t *testing.T) {
	want, err := filepath.Abs("relative/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath("relative/state.json"); got != want {
		t.Fatalf("path=%q want=%q", got, want)
	}
}
