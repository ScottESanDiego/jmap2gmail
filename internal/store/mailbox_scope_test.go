package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailboxScopeRoundTripAndLegacyCompatibility(t *testing.T) {
	file := File{Path: filepath.Join(t.TempDir(), "state.json")}
	state := New()
	state.SourceAccount = "source"
	state.GmailAccount = "gmail"
	state.EmailState = "s1"
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mailbox_scope") {
		t.Fatal("default scope changed legacy JSON shape")
	}
	loaded, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MailboxScope != "" || loaded.Rebuild || loaded.Version != 1 {
		t.Fatalf("legacy state=%#v", loaded)
	}
	state.MailboxScope = `["a","b"]`
	state.Messages["email"] = Message{GmailID: "gmail-email", SourceDeleted: true}
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err = file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MailboxScope != state.MailboxScope || !loaded.Messages["email"].SourceDeleted || loaded.Rebuild || loaded.Version != 1 {
		t.Fatalf("scope roundtrip=%#v", loaded)
	}
}
