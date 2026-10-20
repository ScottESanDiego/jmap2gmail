package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jmap2gmail/internal/store"
)

type resumeSource struct {
	*fakeSource
	inventories int
	closeErr    error
}

func (s *resumeSource) AllEmailIDs(ctx context.Context) ([]string, error) {
	s.inventories++
	return s.fakeSource.AllEmailIDs(ctx)
}
func (s *resumeSource) Raw(ctx context.Context, id string) (io.ReadCloser, error) {
	r, err := s.fakeSource.Raw(ctx, id)
	if err != nil {
		return nil, err
	}
	return &resumeBody{ReadCloser: r, err: s.closeErr}, nil
}

type resumeBody struct {
	io.ReadCloser
	err error
}

func (r *resumeBody) Close() error { return errors.Join(r.ReadCloser.Close(), r.err) }

type resumeDestination struct {
	*fakeDestination
	inventories int
	modify      func(string) error
	insert      func(context.Context, io.Reader, []string) (Message, error)
}

func (d *resumeDestination) Inventory(ctx context.Context) ([]Message, error) {
	d.inventories++
	return d.fakeDestination.Inventory(ctx)
}
func (d *resumeDestination) ModifyLabels(ctx context.Context, id string, add, remove []string) (Message, error) {
	if d.modify != nil {
		if err := d.modify(id); err != nil {
			return Message{}, err
		}
	}
	return d.fakeDestination.ModifyLabels(ctx, id, add, remove)
}
func (d *resumeDestination) Insert(ctx context.Context, r io.Reader, labels []string) (Message, error) {
	if d.insert != nil {
		return d.insert(ctx, r, labels)
	}
	return d.fakeDestination.Insert(ctx, r, labels)
}

func twoResumeMessages() (*resumeSource, *resumeDestination) {
	s := oneMessageSource()
	s.ids = append(s.ids, "e2")
	s.emails["e2"] = Email{ID: "e2", BlobID: "b2", MailboxIDs: map[string]bool{"inbox": true}, MessageIDs: []string{"m2@example.com"}}
	s.raw["b2"] = "Message-ID: <m2@example.com>\r\n\r\nsecond"
	d := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
	for _, id := range s.ids {
		d.messages["g"+id[1:]] = Message{ID: "g" + id[1:], AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{id}, MessageIDHeaders: s.emails[id].MessageIDs}
	}
	return &resumeSource{fakeSource: s}, &resumeDestination{fakeDestination: d}
}
func seedIncrementalResume(t *testing.T, f store.File, s *resumeSource, d *resumeDestination) {
	t.Helper()
	state := store.New()
	state.SourceAccount = testAccountHash()
	state.GmailAccount = "test-gmail@example.com"
	state.EmailState = "s0"
	state.GmailHistoryID = 1
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.SourceEmails = map[string]store.SourceEmail{}
	state.SourceEmailsComplete = true
	names, err := MailboxLabelNames(s.mailboxes, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{mailboxNames: names, Options: Options{Delete: true}}
	state.PolicyFingerprint = e.policyFingerprint()
	for _, id := range s.ids {
		state.Messages[id] = store.Message{GmailID: "g" + id[1:]}
		state.SourceEmails[id] = sourceRecord(s.emails[id])
	}
	state.Mailboxes["inbox"] = store.Mailbox{GmailLabelID: LabelInbox, Name: LabelInbox}
	if err := f.Save(state); err != nil {
		t.Fatal(err)
	}
	s.changes = []Changes{{NewState: s.state, Updated: append([]string(nil), s.ids...)}}
}

func TestFileResumeKnownModifyFailure(t *testing.T) {
	for _, mode := range []string{"full", "incremental"} {
		t.Run(mode, func(t *testing.T) {
			source, destination := twoResumeMessages()
			file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
			if mode == "incremental" {
				seedIncrementalResume(t, file, source, destination)
			}
			rejection := errors.New("Gmail 400 rejected labels")
			destination.modify = func(id string) error {
				if id == "g2" {
					return rejection
				}
				return nil
			}
			_, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
			if !errors.Is(err, rejection) {
				t.Fatalf("error=%v", err)
			}
			paused, err := file.Load()
			if err != nil {
				t.Fatal(err)
			}
			if paused.Rebuild || paused.Resume == nil || paused.Resume.Next != 1 || paused.Messages["e1"].GmailID != "g1" {
				t.Fatalf("not resumable: %#v", paused)
			}
			if paused.EmailState == source.state {
				t.Fatal("cursor passed failed work")
			}
			source.inventories = 0
			destination.inventories = 0
			destination.modified = nil
			destination.modify = nil
			if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if source.inventories != 0 || destination.inventories != 0 || len(destination.inserted) != 0 || len(destination.modified) != 1 || destination.modified[0].id != "g2" {
				t.Fatalf("repeated completed work/inventories: source=%d destination=%d modifications=%v inserts=%v", source.inventories, destination.inventories, destination.modified, destination.inserted)
			}
			done, err := file.Load()
			if err != nil {
				t.Fatal(err)
			}
			if done.Resume != nil || done.Incomplete || done.EmailState != source.state {
				t.Fatalf("not committed: %#v", done)
			}
		})
	}
}

func TestFileResumeCancellationCheckpointsCompletedWork(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	destination.modify = func(id string) error {
		if id == "g1" {
			cancel()
		}
		return nil
	}
	_, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	paused, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if paused.Resume == nil || paused.Resume.Next != 1 {
		t.Fatalf("completed item not checkpointed: %#v", paused)
	}
	source.inventories = 0
	destination.inventories = 0
	destination.modified = nil
	destination.modify = nil
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if source.inventories+destination.inventories != 0 || len(destination.modified) != 1 || destination.modified[0].id != "g2" {
		t.Fatalf("cancel recovery repeated work: %v", destination.modified)
	}
}

func TestFileResumeRolePolicyReevaluatesCompletedMail(t *testing.T) {
	source, destination := twoResumeMessages()
	source.mailboxes[0].Role = "sent"
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("Gmail 400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err == nil {
		t.Fatal("expected rejection")
	}
	if !hasLabelID(destination.messages["g1"], LabelSent) {
		t.Fatal("first item never completed")
	}
	destination.modify = nil
	destination.modified = nil
	source.inventories = 0
	destination.inventories = 0
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true, RoleLabels: map[string]string{"sent": "Fastmail_Sent"}}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1", "g2"} {
		if (id == "g1" && !hasLabelID(destination.messages[id], LabelSent)) || !hasLabelID(destination.messages[id], "label-Fastmail_Sent") {
			t.Fatalf("policy not reevaluated: %#v", destination.messages[id])
		}
	}
	if source.inventories+destination.inventories != 0 || len(destination.inserted) != 0 {
		t.Fatal("policy change rebuilt or reuploaded")
	}
}

func TestFileInsertAmbiguousOutcomeRecovery(t *testing.T) {
	for _, outcome := range []string{"accepted", "absent", "conflict"} {
		t.Run(outcome, func(t *testing.T) {
			source := &resumeSource{fakeSource: oneMessageSource()}
			destination := &resumeDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}}
			file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
			lost := errors.New("connection lost after sending Insert")
			destination.insert = func(ctx context.Context, r io.Reader, labels []string) (Message, error) {
				if outcome != "absent" {
					m, err := destination.fakeDestination.Insert(ctx, r, labels)
					if err != nil {
						return m, err
					}
					if outcome == "conflict" {
						m.ID = "conflict"
						destination.messages[m.ID] = m
					}
				}
				return Message{}, lost
			}
			if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); !errors.Is(err, lost) {
				t.Fatalf("error=%v", err)
			}
			saved, err := file.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Resume == nil || saved.Resume.InsertIntent == nil {
				t.Fatal("missing durable intent")
			}
			destination.insert = nil
			_, err = (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
			if outcome == "accepted" {
				if err != nil {
					t.Fatal(err)
				}
				if len(destination.inserted) != 1 {
					t.Fatal("duplicate insertion")
				}
			} else {
				if err == nil {
					t.Fatal("unresolved outcome succeeded")
				}
				if len(destination.inserted) > 1 || len(destination.trashed) != 0 {
					t.Fatal("unresolved outcome mutated mail")
				}
			}
		})
	}
}

func TestFileInsertCloseFailureKeepsKnownSuccessfulID(t *testing.T) {
	closeFailure := errors.New("raw close failed")
	source := &resumeSource{fakeSource: oneMessageSource(), closeErr: closeFailure}
	destination := &resumeDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}}
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true}}).Run(t.Context()); !errors.Is(err, closeFailure) {
		t.Fatalf("error=%v", err)
	}
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Messages["e1"].GmailID != "inserted-1" || state.Resume == nil || state.Resume.InsertIntent != nil {
		t.Fatalf("known result lost: %#v", state)
	}
	source.closeErr = nil
	source.inventories = 0
	destination.inventories = 0
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.inserted) != 1 || source.inventories+destination.inventories != 0 {
		t.Fatal("known successful insert rebuilt or duplicated")
	}
}

func TestFileResumeSourceChangeProtectsExtraAndSpam(t *testing.T) {
	source, destination := twoResumeMessages()
	destination.messages["extra"] = Message{ID: "extra", MessageIDHeaders: []string{"m3@example.com"}}
	destination.byMessageID = map[string][]Message{"m3@example.com": {destination.messages["extra"]}}
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err == nil {
		t.Fatal("expected failure")
	}
	source.emails["e3"] = Email{ID: "e3", MailboxIDs: map[string]bool{"inbox": true}, MessageIDs: []string{"m3@example.com"}}
	source.changes = []Changes{{NewState: "s2", Created: []string{"e3"}, Destroyed: []string{"e1"}}}
	source.state = "s2"
	message := destination.messages["g1"]
	message.LabelIDs = append(message.LabelIDs, LabelSpam)
	destination.messages["g1"] = message
	destination.modify = nil
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.trashed) != 0 || len(destination.inserted) != 0 || !hasLabelID(destination.messages["g1"], LabelSpam) {
		t.Fatalf("stale cleanup or Spam mutation: %#v", destination.fakeDestination)
	}
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Messages["e3"].GmailID != "extra" {
		t.Fatalf("new live extra not adopted: %#v", state.Messages)
	}
}

func TestDryResumeDeepCloneDoesNotAlterPendingState(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true}}).Run(t.Context()); err == nil {
		t.Fatal("expected rejection")
	}
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	before := state.Clone()
	destination.modify = nil
	source.changes = []Changes{{NewState: "s2", Destroyed: []string{"e1"}}}
	if _, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, state) {
		t.Fatalf("dry run mutated pending state: before=%#v after=%#v", before, state)
	}
}

func TestExplicitInsertRejectionCanRetryWithoutInventory(t *testing.T) {
	source := &resumeSource{fakeSource: oneMessageSource()}
	destination := &resumeDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}}
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.insert = func(context.Context, io.Reader, []string) (Message, error) {
		return Message{}, errors.Join(ErrMutationRejected, errors.New("Gmail 400"))
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true}}).Run(t.Context()); !errors.Is(err, ErrMutationRejected) {
		t.Fatalf("error=%v", err)
	}
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Resume == nil || state.Resume.InsertIntent != nil {
		t.Fatal("definite rejection left ambiguous intent")
	}
	destination.insert = nil
	source.inventories = 0
	destination.inventories = 0
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if source.inventories+destination.inventories != 0 || len(destination.inserted) != 1 {
		t.Fatal("explicit rejection did not resume cleanly")
	}
}

type observedCheckpointStore struct {
	file            store.File
	saves           int
	sourcePositions []int
	reject          func(*store.State) bool
}

func (s *observedCheckpointStore) Load() (*store.State, error) { return s.file.Load() }
func (s *observedCheckpointStore) Save(state *store.State) error {
	s.saves++
	if state.Resume != nil && state.Resume.Phase == "source" {
		s.sourcePositions = append(s.sourcePositions, state.Resume.Next)
	}
	if s.reject != nil && s.reject(state) {
		return errors.New("checkpoint rejected")
	}
	return s.file.Save(state)
}
func TestFileResumeCheckpointsUnchangedWorkInBoundedBatches(t *testing.T) {
	source, destination := twoResumeMessages()
	source.ids = nil
	source.emails = map[string]Email{}
	destination.messages = map[string]Message{}
	for i := range 251 {
		id := fmt.Sprintf("e%03d", i)
		source.ids = append(source.ids, id)
		source.emails[id] = Email{ID: id, MailboxIDs: map[string]bool{"inbox": true}}
		destination.messages[id] = Message{ID: id, LabelIDs: []string{LabelInbox, LabelUnread, testManagedLabel()}, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{id}}
	}
	backend := &observedCheckpointStore{file: store.File{Path: filepath.Join(t.TempDir(), "state.json")}}
	if _, err := (&Engine{Source: source, Destination: destination, Store: backend, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if backend.saves > 14 || !slices.Contains(backend.sourcePositions, 100) || !slices.Contains(backend.sourcePositions, 200) {
		t.Fatalf("checkpoint amplification: saves=%d positions=%v", backend.saves, backend.sourcePositions)
	}
}

func TestFileInsertCheckpointBoundaries(t *testing.T) {
	for _, boundary := range []string{"intent", "result"} {
		t.Run(boundary, func(t *testing.T) {
			source := &resumeSource{fakeSource: oneMessageSource()}
			destination := &resumeDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}}
			backend := &observedCheckpointStore{file: store.File{Path: filepath.Join(t.TempDir(), "state.json")}, reject: func(state *store.State) bool {
				if state.Resume == nil {
					return false
				}
				if boundary == "intent" {
					return state.Resume.InsertIntent != nil
				}
				return state.Resume.InsertIntent == nil && state.Messages["e1"].GmailID != ""
			}}
			if _, err := (&Engine{Source: source, Destination: destination, Store: backend, Options: Options{Apply: true}}).Run(t.Context()); err == nil {
				t.Fatal("expected checkpoint failure")
			}
			if boundary == "intent" && len(destination.inserted) != 0 {
				t.Fatal("Insert ran without durable intent")
			}
			saved, err := backend.file.Load()
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "result" && (saved.Resume == nil || saved.Resume.InsertIntent == nil) {
				t.Fatal("result save failure erased durable intent")
			}
			if _, err := (&Engine{Source: source, Destination: destination, Store: backend.file, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(destination.inserted) != 1 {
				t.Fatalf("insert count=%d", len(destination.inserted))
			}
		})
	}
}

func TestFileResumeContradictoryDestroyCannotTrashKnownLiveMail(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	seedIncrementalResume(t, file, source, destination)
	source.changes = nil
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.Incomplete = true
	state.Resume = &store.Resume{Version: store.ResumeVersion, Mode: "incremental", Phase: "source", SourceCursor: source.state, GmailHistoryID: 1, Work: []store.WorkItem{{SourceID: "e1", Kind: "destroy"}}}
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.trashed) != 0 {
		t.Fatalf("trashed known live source: %v", destination.trashed)
	}
}

func TestLegacyCleanCachePolicyChangeReevaluatesWithoutSourceEvents(t *testing.T) {
	source, destination := twoResumeMessages()
	source.mailboxes[0].Role = "sent"
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	seedIncrementalResume(t, file, source, destination)
	source.changes = nil
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.PolicyFingerprint = ""
	state.SourceEmailsComplete = false
	state.SourceEmails = nil
	state.Mailboxes["inbox"] = store.Mailbox{GmailLabelID: LabelSent, Name: LabelSent}
	for id, message := range destination.messages {
		message.LabelIDs = []string{LabelSent, testManagedLabel()}
		destination.messages[id] = message
	}
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true, RoleLabels: map[string]string{"sent": "Fastmail_Sent"}}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if source.inventories != 1 || destination.inventories != 1 {
		t.Fatal("legacy policy change did not safely rebuild missing metadata")
	}
	for _, message := range destination.messages {
		if !hasLabelID(message, LabelSent) || !hasLabelID(message, "label-Fastmail_Sent") {
			t.Fatalf("legacy completed message ignored new policy: %#v", message)
		}
	}
	if len(destination.inserted) != 0 || len(destination.trashed) != 0 {
		t.Fatal("policy change duplicated or deleted mail")
	}
}

func TestFileResumeMissingGmailHistoryMetadataRequiresCompletePreflight(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err == nil {
		t.Fatal("expected rejection")
	}
	before, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	// The existing complete source view still lists e1, but Email/get omits its
	// metadata. A Gmail-side label change cannot prove that source deletion.
	delete(source.emails, "e1")
	destination.modify = nil
	history := &fakeHistoryDestination{fakeDestination: destination.fakeDestination, currentHistory: 2, historyIDs: []string{"g1"}}
	_, err = (&Engine{Source: source, Destination: history, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "snapshot was incomplete") {
		t.Fatalf("error=%v, want failed complete preflight", err)
	}
	after, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(destination.trashed) != 0 || after.Messages["e1"].GmailID != "g1" || after.SourceEmails["e1"].ID != "e1" || after.Resume.GmailHistoryID != before.Resume.GmailHistoryID {
		t.Fatalf("missing metadata authorized deletion or advanced history: %#v", after)
	}
}

func TestInitialFullDefersDestroyUntilSourceCatchup(t *testing.T) {
	source, destination := twoResumeMessages()
	source.ids = []string{"e1"}
	source.changes = []Changes{{NewState: "s2", Created: []string{"e2"}}}
	source.state = "s2"
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.trashed) != 0 || len(destination.inserted) != 0 {
		t.Fatalf("stale initial snapshot caused destructive work: trash=%v insert=%v", destination.trashed, destination.inserted)
	}
}

func TestFailedFullPreflightPreservesPriorJournalAndScope(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: false}}).Run(t.Context()); err == nil {
		t.Fatal("expected initial pause")
	}
	before, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	original := destination.messages["g1"]
	delete(destination.messages, "g1")
	destination.messages["ambiguous-a"] = Message{ID: "ambiguous-a", MessageIDHeaders: []string{"m1@example.com"}}
	destination.messages["ambiguous-b"] = Message{ID: "ambiguous-b", MessageIDHeaders: []string{"m1@example.com"}}
	// Changing deletion policy changes the source-view fingerprint. Failed new
	// planning must not bind the old pending work to that new fingerprint.
	_, err = (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error=%v", err)
	}
	after, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed preflight published partial plan:\nbefore=%#v\nafter=%#v", before, after)
	}
	destination.messages["g1"] = original
	delete(destination.messages, "ambiguous-a")
	delete(destination.messages, "ambiguous-b")
	destination.modify = nil
	source.inventories = 0
	destination.inventories = 0
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if source.inventories != 1 || destination.inventories != 1 {
		t.Fatal("changed view resumed old journal after failed preflight")
	}
}

func TestFullProgressExcludesDeferredDestroyedWork(t *testing.T) {
	source, destination := twoResumeMessages()
	source.ids = []string{"e1"}
	delete(source.emails, "e2")
	var output bytes.Buffer
	if _, err := (&Engine{Source: source, Destination: destination, Store: store.File{Path: filepath.Join(t.TempDir(), "state.json")}, Options: Options{Apply: true, Delete: true, Output: &output}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "full Fastmail snapshot (0/1 messages") || !strings.Contains(text, "full Fastmail snapshot (1/1 messages") || strings.Contains(text, "/2 messages") {
		t.Fatalf("untruthful progress:\n%s", text)
	}
}

func TestFileResumeCustomRolesNeverRemoveNativeSentOrDraft(t *testing.T) {
	for _, role := range []string{"sent", "drafts"} {
		for _, targetExists := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/targetExists=%t", role, targetExists), func(t *testing.T) {
				source, destination := twoResumeMessages()
				source.mailboxes[0].Role = role
				native := LabelSent
				if role == "drafts" {
					native = LabelDraft
				}
				for id, message := range destination.messages {
					message.LabelIDs = []string{native, testManagedLabel()}
					destination.messages[id] = message
				}
				custom := "Fastmail_" + role
				customID := "label-" + custom
				if targetExists {
					customID = "existing-custom"
					destination.labels = append(destination.labels, Label{ID: customID, Name: custom, Type: "user"})
				}
				file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
				destination.modify = func(id string) error {
					if id == "g2" {
						return errors.New("400")
					}
					return nil
				}
				if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err == nil {
					t.Fatal("expected pause")
				}
				source.inventories = 0
				destination.inventories = 0
				destination.modify = nil
				destination.modified = nil
				if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true, RoleLabels: map[string]string{role: custom}}}).Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				for _, call := range destination.modified {
					if slices.Contains(call.remove, LabelSent) || slices.Contains(call.remove, LabelDraft) {
						t.Fatalf("unsupported native removal: %#v", call)
					}
				}
				for _, message := range destination.messages {
					if !hasLabelID(message, native) || !hasLabelID(message, customID) {
						t.Fatalf("custom/native migration=%#v", message)
					}
				}
				if source.inventories+destination.inventories != 0 || len(destination.inserted)+len(destination.trashed) != 0 {
					t.Fatal("native role migration rebuilt/deleted/reinserted mail")
				}
			})
		}
	}
}

func TestFileResumeUserRoleLabelRenamesInPlace(t *testing.T) {
	source, destination := twoResumeMessages()
	source.mailboxes[0].Role = "trash"
	destination.labels = append(destination.labels, Label{ID: "old-user-label", Name: "Fastmail_Trash", Type: "user"})
	for id, message := range destination.messages {
		message.LabelIDs = []string{"old-user-label", testManagedLabel()}
		destination.messages[id] = message
	}
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	destination.modify = func(id string) error {
		if id == "g2" {
			return errors.New("400")
		}
		return nil
	}
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err == nil {
		t.Fatal("expected pause")
	}
	source.inventories = 0
	destination.inventories = 0
	destination.modify = nil
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true, RoleLabels: map[string]string{"trash": "Custom_Trash"}}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(destination.renamed, []string{"old-user-label:Custom_Trash"}) {
		t.Fatalf("rename=%v", destination.renamed)
	}
	for _, message := range destination.messages {
		if !hasLabelID(message, "old-user-label") {
			t.Fatal("rename replaced existing membership ID")
		}
	}
	if source.inventories+destination.inventories != 0 || len(destination.inserted)+len(destination.trashed) != 0 {
		t.Fatal("user role rename rebuilt/deleted/reinserted mail")
	}
}

func TestDeleteDisabledDoesNotReprocessRetainedTombstonesWithoutChanges(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: filepath.Join(t.TempDir(), "state.json")}
	seedIncrementalResume(t, file, source, destination)
	source.changes = nil
	state, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.MailboxScope = mailboxScopeFingerprint(nil, false)
	names, err := MailboxLabelNames(source.mailboxes, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.PolicyFingerprint = (&Engine{mailboxNames: names}).policyFingerprint()
	state.Messages["e1"] = store.Message{GmailID: "g1", SourceDeleted: true}
	delete(state.SourceEmails, "e1")
	if err := file.Save(state); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: false}}).Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if result.SkippedTrash != 0 || len(destination.modified)+len(destination.trashed)+len(destination.inserted) != 0 {
			t.Fatalf("unchanged retained tombstone was reprocessed: %#v", result)
		}
	}
}
