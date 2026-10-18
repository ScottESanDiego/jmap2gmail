package main

import (
	"bytes"
	"errors"
	"flag"
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
		"    \tpath to configuration JSON (default \"config.json\")\n" +
		"  -dry-run\n" +
		"    \tpreview changes without writing Gmail, cache, locks, or OAuth tokens\n" +
		"  -full\n" +
		"    \tscan all Fastmail mail instead of using the JMAP cursor\n" +
		"  -no-cache\n" +
		"    \trebuild from servers; create no state or state-lock files\n"
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
