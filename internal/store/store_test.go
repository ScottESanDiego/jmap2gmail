package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFileRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	file := File{Path: path}
	state := New()
	state.SourceAccount = "fastmail-account"
	state.GmailAccount = "gmail@example.com"
	state.EmailState = "s1"
	state.Messages["fastmail-1"] = Message{GmailID: "gmail-1"}
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state permissions = %o, want 600", got)
	}
	loaded, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, state) {
		t.Fatalf("loaded state = %#v, want %#v", loaded, state)
	}
}

func TestMissingFileReturnsEmptyState(t *testing.T) {
	state, err := (File{Path: filepath.Join(t.TempDir(), "missing.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != CurrentVersion || len(state.Messages) != 0 || len(state.Mailboxes) != 0 {
		t.Fatalf("unexpected empty state: %#v", state)
	}
	if !state.Rebuild {
		t.Fatal("missing state did not request a remote rebuild")
	}
}

func TestCorruptAndInvalidStateRequestRebuild(t *testing.T) {
	for name, data := range map[string]string{
		"corrupt": `{`,
		"invalid": `{"version":1,"messages":{"e1":{"gmail_id":""}},"mailboxes":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			state, err := (File{Path: path}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if !state.Rebuild || state.RebuildReason == "" {
				t.Fatalf("state = %#v, want classified rebuild", state)
			}
		})
	}
}

func TestSemanticallyIncompleteStateRequestsRebuild(t *testing.T) {
	valid := `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{}}`
	tests := map[string]string{
		"version only":           `{"version":1}`,
		"missing messages":       `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","mailboxes":{}}`,
		"missing mailboxes":      `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{}}`,
		"missing source account": `{"version":1,"gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{}}`,
		"missing Gmail account":  `{"version":1,"source_account":"fastmail-account","email_state":"s1","messages":{},"mailboxes":{}}`,
		"missing email state":    `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","messages":{},"mailboxes":{}}`,
		"empty source ID":        `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{"":{"gmail_id":"g1"}},"mailboxes":{}}`,
		"empty Gmail ID":         `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{"e1":{"gmail_id":""}},"mailboxes":{}}`,
		"duplicate Gmail ID":     `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{"e1":{"gmail_id":"g1"},"e2":{"gmail_id":"g1"}},"mailboxes":{}}`,
		"empty mailbox ID":       `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{"":{"gmail_label_id":"l1","name":"Label"}}}`,
		"empty label ID":         `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{"m1":{"gmail_label_id":"","name":"Label"}}}`,
		"empty mailbox name":     `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{"m1":{"gmail_label_id":"l1","name":""}}}`,
		"duplicate label ID":     `{"version":1,"source_account":"fastmail-account","gmail_account":"gmail@example.com","email_state":"s1","messages":{},"mailboxes":{"m1":{"gmail_label_id":"l1","name":"One"},"m2":{"gmail_label_id":"l1","name":"Two"}}}`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			state, err := (File{Path: path}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if !state.Rebuild || state.RebuildReason != "is invalid" {
				t.Fatalf("state = %#v, want invalid rebuild", state)
			}
		})
	}

	t.Run("valid empty completed state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		state, err := (File{Path: path}).Load()
		if err != nil {
			t.Fatal(err)
		}
		if state.Rebuild || len(state.Messages) != 0 || len(state.Mailboxes) != 0 {
			t.Fatalf("state = %#v, want valid empty completed state", state)
		}
	})
}

func TestStateReadErrorIsNotClassifiedAsCorruption(t *testing.T) {
	path := t.TempDir()
	state, err := (File{Path: path}).Load()
	if err == nil || state != nil || !strings.Contains(err.Error(), "read state") {
		t.Fatalf("state = %#v, error = %v", state, err)
	}
}

func TestMessageCacheRoundTripContainsOnlyEssentialFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := New()
	state.SourceAccount = "fastmail-account"
	state.GmailAccount = "gmail@example.com"
	state.EmailState = "s1"
	state.Messages["e1"] = Message{GmailID: "g1", SourceDeleted: true}
	if err := (File{Path: path}).Save(state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"gmail_id": "g1"`) || !strings.Contains(text, `"source_deleted": true`) {
		t.Fatalf("state JSON lost essential fields: %s", text)
	}
	for _, obsolete := range []string{"blob_id", "message_id", "desired_labels"} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("state JSON retains obsolete field %q: %s", obsolete, text)
		}
	}
}

func TestAcquireLockRejectsOverlappingRun(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nested", "state.json")
	first, err := AcquireLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if info, err := os.Stat(statePath + ".lock"); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock permissions = %o, want 600", got)
	}

	second, err := AcquireLock(statePath)
	if err == nil || !strings.Contains(err.Error(), "another jmap2gmail") {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second lock error = %v, want contention", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireLock(statePath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
