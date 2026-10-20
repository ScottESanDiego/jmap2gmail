package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectorySyncFailurePreservesRenamedIntent(t *testing.T) {
	s := resumeFixture()
	s.Resume.InsertIntent = &InsertIntent{SourceID: "e1", Account: "account", MessageID: "id@example.com"}
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	want := errors.New("simulated directory sync failure")
	called := false
	err := f.save(s, func(dir string) error {
		called = true
		if dir != filepath.Dir(f.Path) {
			t.Fatalf("synced %q instead of parent directory", dir)
		}
		loaded, err := f.Load()
		if err != nil || loaded.Resume == nil || loaded.Resume.InsertIntent == nil {
			t.Fatalf("sync preceded rename: state=%#v error=%v", loaded, err)
		}
		return want
	})
	if !called || !errors.Is(err, want) || !strings.Contains(err.Error(), "sync state directory") {
		t.Fatalf("directory sync error not propagated: %v", err)
	}
	loaded, err := f.Load()
	if err != nil || loaded.Resume == nil || !reflect.DeepEqual(loaded.Resume.InsertIntent, s.Resume.InsertIntent) {
		t.Fatalf("post-rename failure lost intent: %#v, %v", loaded, err)
	}
	entries, err := os.ReadDir(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("unexpected post-failure files: %v", entries)
	}
}

func resumeFixture() *State {
	s := New()
	s.SourceAccount = "account"
	s.GmailAccount = "gmail@example.com"
	s.MailboxScope = "scope"
	s.PolicyFingerprint = "labels"
	s.Incomplete = true
	s.SourceEmailsComplete = true
	s.SourceEmails = map[string]SourceEmail{"e1": {ID: "e1", BlobID: "b1", MailboxIDs: map[string]bool{"m1": true}, Keywords: map[string]bool{"$seen": true}, MessageIDs: []string{"id@example.com"}}}
	s.Resume = &Resume{Version: ResumeVersion, Mode: "full", Phase: "source", SourceCursor: "s1", GmailHistoryID: 10, Work: []WorkItem{{SourceID: "e1", Kind: "reconcile"}}, ExtraIDs: []string{"g-extra"}, OldManagedLabelIDs: []string{"l-old"}}
	return s
}

func roundTripResume(t *testing.T, s *State) *State {
	t.Helper()
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := f.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestResumeEveryPhaseReloadsInitialFullWithoutCommittedCursor(t *testing.T) {
	for _, phase := range []string{"labels", "source", "catchup", "caught-up", "extras", "commit"} {
		t.Run(phase, func(t *testing.T) {
			s := resumeFixture()
			s.Resume.Phase = phase
			if phase == "extras" || phase == "commit" {
				s.Resume.Next = len(s.Resume.Work)
			}
			if phase == "commit" {
				s.Resume.ExtraNext = len(s.Resume.ExtraIDs)
			}
			loaded := roundTripResume(t, s)
			if loaded.Rebuild || !reflect.DeepEqual(s, loaded) {
				t.Fatalf("resume was not preserved: %#v", loaded)
			}
		})
	}
}

func TestIncrementalResumeReloads(t *testing.T) {
	s := resumeFixture()
	s.Resume.Mode = "incremental"
	s.EmailState = "s0"
	s.SourceEmailsComplete = false
	if loaded := roundTripResume(t, s); loaded.Rebuild || loaded.Resume == nil {
		t.Fatalf("invalid incremental reload: %#v", loaded)
	}
}

func TestMalformedResumeRequestsRebuild(t *testing.T) {
	tests := map[string]func(*State){
		"version":                        func(s *State) { s.Resume.Version++ },
		"phase":                          func(s *State) { s.Resume.Phase = "unknown" },
		"mode":                           func(s *State) { s.Resume.Mode = "unknown" },
		"not incomplete":                 func(s *State) { s.Incomplete = false },
		"missing baseline":               func(s *State) { s.Resume.SourceCursor = "" },
		"missing history":                func(s *State) { s.Resume.GmailHistoryID = 0 },
		"missing policy":                 func(s *State) { s.PolicyFingerprint = "" },
		"missing scope":                  func(s *State) { s.MailboxScope = "" },
		"missing complete metadata":      func(s *State) { s.SourceEmailsComplete = false },
		"negative next":                  func(s *State) { s.Resume.Next = -1 },
		"past next":                      func(s *State) { s.Resume.Next = 2 },
		"negative extra next":            func(s *State) { s.Resume.ExtraNext = -1 },
		"past extra next":                func(s *State) { s.Resume.ExtraNext = 2 },
		"duplicate work":                 func(s *State) { s.Resume.Work = append(s.Resume.Work, s.Resume.Work[0]) },
		"conflicting work":               func(s *State) { s.Resume.Work = append(s.Resume.Work, WorkItem{SourceID: "e1", Kind: "destroy"}) },
		"unknown kind":                   func(s *State) { s.Resume.Work[0].Kind = "upload" },
		"missing metadata":               func(s *State) { delete(s.SourceEmails, "e1") },
		"metadata identity":              func(s *State) { e := s.SourceEmails["e1"]; e.ID = "other"; s.SourceEmails["e1"] = e },
		"duplicate extras":               func(s *State) { s.Resume.ExtraIDs = []string{"g", "g"} },
		"duplicate labels":               func(s *State) { s.Resume.OldManagedLabelIDs = []string{"l", "l"} },
		"incremental missing old cursor": func(s *State) { s.Resume.Mode = "incremental" },
		"extras skips work":              func(s *State) { s.Resume.Phase = "extras" },
		"commit skips extras":            func(s *State) { s.Resume.Phase = "commit"; s.Resume.Next = len(s.Resume.Work) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := resumeFixture()
			mutate(s)
			if loaded := roundTripResume(t, s); !loaded.Rebuild {
				t.Fatalf("malformed resume accepted: %#v", loaded)
			}
		})
	}
}

func TestInsertIntentSurvivesRoundTripAndInvalidJournal(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		s := resumeFixture()
		s.Resume.InsertIntent = &InsertIntent{SourceID: "e1", Account: "account", MessageID: "id@example.com"}
		if invalid {
			s.Resume.Version++
			s.Messages["e1"] = Message{GmailID: "duplicate"}
			s.Messages["e2"] = Message{GmailID: "duplicate"}
		}
		loaded := roundTripResume(t, s)
		if loaded.Rebuild != invalid || loaded.Resume == nil || !reflect.DeepEqual(loaded.Resume.InsertIntent, s.Resume.InsertIntent) {
			t.Fatalf("lost recovery identity: %#v", loaded)
		}
		if invalid && (len(loaded.Messages) != 0 || len(loaded.Mailboxes) != 0 || len(loaded.SourceEmails) != 0 || len(loaded.Resume.Work) != 0) {
			t.Fatalf("invalid journal remained trusted: %#v", loaded)
		}
	}
}

func TestLegacyIncompleteStillRebuilds(t *testing.T) {
	s := New()
	s.SourceAccount = "account"
	s.GmailAccount = "gmail@example.com"
	s.EmailState = "s0"
	s.Incomplete = true
	loaded := roundTripResume(t, s)
	if !loaded.Rebuild || loaded.Resume != nil || loaded.RebuildReason != "was not committed" {
		t.Fatalf("legacy incomplete not rebuilt: %#v", loaded)
	}
}

func TestEmptyCompleteSourceMetadataSurvivesReload(t *testing.T) {
	s := resumeFixture()
	s.SourceEmails = map[string]SourceEmail{}
	s.Resume.Work = nil
	loaded := roundTripResume(t, s)
	if loaded.Rebuild || !loaded.SourceEmailsComplete || loaded.SourceEmails == nil {
		t.Fatalf("lost empty complete metadata: %#v", loaded)
	}
}

func TestInvalidUncertainIdentityFailsClosed(t *testing.T) {
	s := resumeFixture()
	s.Resume.InsertIntent = &InsertIntent{SourceID: "e1", Account: "wrong", MessageID: "id@example.com"}
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := f.Save(s); err != nil {
		t.Fatal(err)
	}
	if loaded, err := f.Load(); err == nil || loaded != nil {
		t.Fatalf("invalid identity was discarded or accepted: %#v, %v", loaded, err)
	}
}

func TestStateCloneDoesNotAliasMetadataOrResume(t *testing.T) {
	s := resumeFixture()
	s.Messages["e1"] = Message{GmailID: "g1"}
	s.Mailboxes["m1"] = Mailbox{GmailLabelID: "l1", Name: "Inbox"}
	s.Resume.InsertIntent = &InsertIntent{SourceID: "e1", Account: "account", MessageID: "id@example.com"}
	before := s.Clone()
	c := s.Clone()
	c.Messages["e1"] = Message{GmailID: "other"}
	c.Mailboxes["m1"] = Mailbox{Name: "other"}
	e := c.SourceEmails["e1"]
	e.MailboxIDs["m1"] = false
	e.Keywords["$seen"] = false
	e.MessageIDs[0] = "other"
	c.SourceEmails["new"] = SourceEmail{ID: "new"}
	c.Resume.Work[0].SourceID = "other"
	c.Resume.ExtraIDs[0] = "other"
	c.Resume.OldManagedLabelIDs[0] = "other"
	c.Resume.InsertIntent.Account = "other"
	if !reflect.DeepEqual(s, before) {
		t.Fatal("clone mutation changed original state or recovery identity")
	}
}
