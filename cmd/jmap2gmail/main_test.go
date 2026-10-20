package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncHelpStatesOperationalContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"sync", "-h"}, &stdout, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("sync -h error = %v, want flag.ErrHelp", err)
	}
	want := syncHelp + "  -config string\n" +
		"    \tpath to the JSON config file (default \"config.json\")\n" +
		"  -dry-run\n" +
		"    \tpreview changes without Gmail, cache, lock, or OAuth token writes\n" +
		"  -full\n" +
		"    \tscan the full Fastmail view instead of using saved change history\n" +
		"  -no-cache\n" +
		"    \trebuild from servers without a state cache or state lock\n" +
		"  -verbose\n" +
		"    \tshow actions, progress, retry waits, configuration, and folder diagnostics on stderr\n"
	if got := stderr.String(); got != want {
		t.Fatalf("sync -h =\n%q\nwant\n%q", got, want)
	}
}

func TestTopLevelHelpDescribesSyncAndDryRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sync         mirror Fastmail to Gmail; add --dry-run to preview") {
		t.Fatalf("help does not describe sync and dry-run behavior:\n%s", stdout.String())
	}
}

func TestNoCacheBackendCreatesNoStateOrLockArtifacts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "cache.json")
	backend, closeBackend, err := syncStateBackend(true, true, path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := backend.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Save(state); err != nil {
		t.Fatal(err)
	}
	closeBackend()
	for _, artifact := range []string{filepath.Dir(path), path, path + ".lock"} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("cacheless backend created %q", artifact)
		}
	}
}

func TestDryRunBackendReadsWithoutCreatingLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	backend, closeBackend, err := syncStateBackend(false, false, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	if _, err := backend.Load(); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{path, path + ".lock"} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("dry-run backend created %q", artifact)
		}
	}
}

func TestSyncDefaultAppliesAndDryRunCreatesNoLock(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantLock bool
	}{
		{name: "default sync", wantLock: true},
		{name: "dry run", args: []string{"--dry-run"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "config.json")
			// Fail before any network calls, after the selected state backend opens.
			config := `{"fastmail":{"token_file":"missing-fastmail-token"},"gmail":{"credentials_file":"client.json","token_file":"token.json"},"state_file":"state/cache.json"}`
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"sync", "--config", configPath}, test.args...)
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "read Fastmail API token") {
				t.Fatalf("sync error = %v", err)
			}
			lockPath := filepath.Join(root, "state", "cache.json.lock")
			_, err = os.Stat(lockPath)
			if test.wantLock && err != nil {
				t.Fatalf("default sync did not acquire state lock: %v", err)
			}
			if !test.wantLock && !os.IsNotExist(err) {
				t.Fatalf("dry run created state lock: %v", err)
			}
		})
	}
}

func TestInvalidMailboxSelectionFailsBeforePersistentWrites(t *testing.T) {
	for _, selector := range []string{"Projects/Typo", "Projects/Active/*", "Missing/*", "Projects/*/Child", "/*"} {
		t.Run(selector, func(t *testing.T) {
			for _, args := range [][]string{nil, {"--dry-run"}, {"--no-cache"}, {"--dry-run", "--no-cache"}, {"--full"}, {"--full", "--dry-run"}, {"--full", "--no-cache"}, {"--full", "--dry-run", "--no-cache"}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					root := t.TempDir()
					apiCalls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") != "Bearer fastmail-secret" {
							t.Error("missing Fastmail bearer")
						}
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodGet {
							io.WriteString(w, `{"capabilities":{"urn:ietf:params:jmap:core":{},"urn:ietf:params:jmap:mail":{}},"primaryAccounts":{"urn:ietf:params:jmap:mail":"account-1"},"username":"user@example.com","apiUrl":"http://`+r.Host+`/api","downloadUrl":"http://`+r.Host+`/download/{accountId}/{blobId}/{name}","state":"s1"}`)
							return
						}
						apiCalls++
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if !strings.Contains(string(data), `"Mailbox/get"`) {
							t.Errorf("unexpected API request: %s", data)
						}
						io.WriteString(w, `{"methodResponses":[["Mailbox/get",{"accountId":"account-1","state":"m1","list":[{"id":"p","name":"Projects"},{"id":"a","name":"Active","parentId":"p"}]},"0"]],"sessionState":"s1"}`)
					}))
					defer server.Close()
					configPath := filepath.Join(root, "config.json")
					content := map[string]any{
						"fastmail": map[string]any{"token_file": "fastmail-token", "session_endpoint": server.URL + "/session", "include_mailboxes": []string{selector}},
						"gmail":    map[string]any{"credentials_file": "missing-client.json", "token_file": "google-token.json"}, "state_file": "state/cache.json",
					}
					data, err := json.Marshal(content)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(configPath, data, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "fastmail-token"), []byte("fastmail-secret"), 0600); err != nil {
						t.Fatal(err)
					}
					tokenPath := filepath.Join(root, "google-token.json")
					original := []byte("existing OAuth token must remain untouched")
					if err := os.WriteFile(tokenPath, original, 0600); err != nil {
						t.Fatal(err)
					}
					var stdout, stderr bytes.Buffer
					command := append([]string{"sync", "--config", configPath}, args...)
					err = run(t.Context(), command, &stdout, &stderr)
					if err == nil || (!strings.Contains(err.Error(), selector) || !strings.Contains(err.Error(), "fastmail.include_mailboxes")) {
						t.Fatalf("expected path resolution error, got %v", err)
					}
					if apiCalls != 1 {
						t.Fatalf("Fastmail API calls=%d want mailbox preflight only", apiCalls)
					}
					if got, err := os.ReadFile(tokenPath); err != nil || !bytes.Equal(got, original) {
						t.Fatalf("OAuth token changed: %q error %v", got, err)
					}
					for _, artifact := range []string{"state", "state/cache.json", "state/cache.json.lock", "google-token.json.lock"} {
						if _, err := os.Stat(filepath.Join(root, artifact)); !os.IsNotExist(err) {
							t.Fatalf("preflight created %s: %v", artifact, err)
						}
					}
				})
			}
		})
	}
}

func TestHelpUsesToolCommandName(t *testing.T) {
	for _, command := range []string{"help", "sync", "google-auth"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := []string{command}
			if command != "help" {
				args = append(args, "--help")
			}
			err := run(t.Context(), args, &stdout, &stderr)
			if command == "help" && err != nil || command != "help" && !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("help error=%v", err)
			}
			output := stdout.String() + stderr.String()
			want := "Usage: jmap2gmail "
			if command != "help" {
				want += command + " "
			}
			if !strings.Contains(output, want) {
				t.Fatalf("help lacks %q:\n%s", want, output)
			}
		})
	}
}
