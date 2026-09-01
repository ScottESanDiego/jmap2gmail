package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncHelpStatesOperationalContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"sync", "-h"}, &stdout, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("sync -h error = %v, want flag.ErrHelp", err)
	}
	want := syncHelp + `  -apply
    	apply planned Gmail changes; cache them unless --no-cache
  -config string
    	path to configuration JSON (default "config.json")
  -full
    	scan all Fastmail mail instead of using the JMAP cursor
  -no-cache
    	rebuild from servers; create no state or state-lock files
`
	if got := stderr.String(); got != want {
		t.Fatalf("sync -h =\n%q\nwant\n%q", got, want)
	}
}

func TestTopLevelHelpSaysSyncPlansByDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sync         plan a mirror run") {
		t.Fatalf("help does not describe sync planning:\n%s", stdout.String())
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
