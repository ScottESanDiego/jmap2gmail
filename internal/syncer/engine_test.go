package syncer

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/mail"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jmapsync/internal/store"
)

type fakeSource struct {
	mailboxes []Mailbox
	state     string
	stateErr  error
	ids       []string
	emails    map[string]Email
	changes   []Changes
	changeErr error
	raw       map[string]string
}

func (f *fakeSource) SourceIdentity(context.Context) (string, error) {
	return "test-user|test-account", nil
}

type identifiedSource struct {
	*fakeSource
	identity string
}

func (f *identifiedSource) SourceIdentity(context.Context) (string, error) {
	return f.identity, nil
}

func (f *fakeSource) Mailboxes(context.Context) ([]Mailbox, error) { return f.mailboxes, nil }
func (f *fakeSource) State(context.Context) (string, error)        { return f.state, f.stateErr }
func (f *fakeSource) AllEmailIDs(context.Context) ([]string, error) {
	return append([]string(nil), f.ids...), nil
}
func (f *fakeSource) Emails(_ context.Context, ids []string) ([]Email, error) {
	var result []Email
	for _, id := range ids {
		if email, ok := f.emails[id]; ok {
			result = append(result, email)
		}
	}
	return result, nil
}
func (f *fakeSource) Changes(context.Context, string) (Changes, error) {
	if f.changeErr != nil {
		err := f.changeErr
		f.changeErr = nil
		return Changes{}, err
	}
	if len(f.changes) == 0 {
		return Changes{NewState: f.state}, nil
	}
	change := f.changes[0]
	f.changes = f.changes[1:]
	return change, nil
}
func (f *fakeSource) Raw(_ context.Context, blob string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.raw[blob])), nil
}

type modifyCall struct {
	id          string
	add, remove []string
}

type fakeDestination struct {
	labels         []Label
	messages       map[string]Message
	byMessageID    map[string][]Message
	messageIDPages map[string][][]Message
	created        []string
	renamed        []string
	inserted       [][]string
	insertedRaw    []string
	modified       []modifyCall
	trashed        []string
	trashErr       error
	nextID         int
}

func (f *fakeDestination) Labels(context.Context) ([]Label, error) { return f.labels, nil }
func (f *fakeDestination) DestinationIdentity(context.Context) (string, error) {
	return "test-gmail@example.com", nil
}
func (f *fakeDestination) CreateLabel(_ context.Context, name string) (Label, error) {
	label := Label{ID: "label-" + name, Name: name, Type: "user"}
	f.labels = append(f.labels, label)
	f.created = append(f.created, name)
	return label, nil
}
func (f *fakeDestination) RenameLabel(_ context.Context, id, name string) (Label, error) {
	f.renamed = append(f.renamed, id+":"+name)
	return Label{ID: id, Name: name, Type: "user"}, nil
}
func (f *fakeDestination) FindByMessageID(_ context.Context, id string) ([]Message, error) {
	if pages, ok := f.messageIDPages[id]; ok {
		var result []Message
		for _, page := range pages {
			result = append(result, page...)
		}
		return result, nil
	}
	return append([]Message(nil), f.byMessageID[id]...), nil
}
func (f *fakeDestination) CurrentHistoryID(context.Context) (uint64, error)       { return 1, nil }
func (f *fakeDestination) ChangesSince(context.Context, uint64) ([]string, error) { return nil, nil }
func (f *fakeDestination) Inventory(context.Context) ([]Message, error) {
	byID := make(map[string]Message)
	maps.Copy(byID, f.messages)
	add := func(messageID string, messages []Message) {
		for _, message := range messages {
			if len(message.MessageIDHeaders) == 0 {
				message.MessageIDHeaders = []string{messageID}
			}
			if _, exists := byID[message.ID]; !exists {
				byID[message.ID] = message
			} else {
				original := byID[message.ID]
				if len(original.MessageIDHeaders) == 0 {
					original.MessageIDHeaders = message.MessageIDHeaders
					byID[message.ID] = original
				}
			}
		}
	}
	for id, messages := range f.byMessageID {
		add(id, messages)
	}
	for id, pages := range f.messageIDPages {
		for _, messages := range pages {
			add(id, messages)
		}
	}
	var result []Message
	for _, message := range byID {
		result = append(result, message)
	}
	slices.SortFunc(result, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	return result, nil
}

func (f *fakeDestination) ManagedMessages(_ context.Context, labelID string) ([]Message, error) {
	var result []Message
	for _, message := range f.messages {
		if hasLabelID(message, labelID) {
			result = append(result, message)
		}
	}
	slices.SortFunc(result, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	return result, nil
}
func (f *fakeDestination) Message(_ context.Context, id string) (Message, error) {
	message, ok := f.messages[id]
	if !ok {
		return Message{}, ErrNotFound
	}
	return message, nil
}
func (f *fakeDestination) Insert(_ context.Context, raw io.Reader, labels []string) (Message, error) {
	data, _ := io.ReadAll(raw)
	f.nextID++
	id := "inserted-" + string(rune('0'+f.nextID))
	message := Message{ID: id, LabelIDs: append([]string(nil), labels...)}
	parsed, err := mail.ReadMessage(strings.NewReader(string(data)))
	if err == nil {
		message.AccountHeaders = fakeHeaderValues(parsed.Header, HeaderAccount)
		message.EmailIDHeaders = fakeHeaderValues(parsed.Header, HeaderEmailID)
		message.MessageIDHeaders = fakeHeaderValues(parsed.Header, "Message-ID")
	}
	f.messages[id] = message
	f.inserted = append(f.inserted, append([]string(nil), labels...))
	f.insertedRaw = append(f.insertedRaw, string(data))
	return message, nil
}

func fakeHeaderValues(header mail.Header, name string) []string {
	for key, values := range header {
		if strings.EqualFold(key, name) {
			return append([]string(nil), values...)
		}
	}
	return nil
}

type fakeHistoryDestination struct {
	*fakeDestination
	currentHistory uint64
	historyIDs     []string
	historyErr     error
}

func (f *fakeHistoryDestination) CurrentHistoryID(context.Context) (uint64, error) {
	return f.currentHistory, nil
}

func (f *fakeHistoryDestination) ChangesSince(context.Context, uint64) ([]string, error) {
	return append([]string(nil), f.historyIDs...), f.historyErr
}
func (f *fakeDestination) ModifyLabels(_ context.Context, id string, add, remove []string) (Message, error) {
	f.modified = append(f.modified, modifyCall{id: id, add: append([]string(nil), add...), remove: append([]string(nil), remove...)})
	message := f.messages[id]
	set := sliceSet(message.LabelIDs)
	for _, label := range add {
		set[label] = true
	}
	for _, label := range remove {
		delete(set, label)
	}
	message.LabelIDs = nil
	for label := range set {
		message.LabelIDs = append(message.LabelIDs, label)
	}
	slices.Sort(message.LabelIDs)
	f.messages[id] = message
	return message, nil
}
func (f *fakeDestination) Trash(_ context.Context, id string) error {
	if f.trashErr != nil {
		return f.trashErr
	}
	f.trashed = append(f.trashed, id)
	message := f.messages[id]
	message.LabelIDs = append(message.LabelIDs, LabelTrash)
	f.messages[id] = message
	return nil
}

func TestTrashFailurePreservesMappedMessageState(t *testing.T) {
	source := &fakeSource{
		state: "s2", changes: []Changes{{NewState: "s2", Destroyed: []string{"e1"}}},
		emails: map[string]Email{},
	}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1"}},
		trashErr: errors.New("Gmail rejected trash request"),
	}
	state := store.New()
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.EmailState = "s1"
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}

	_, err := (&Engine{
		Source: source, Destination: destination, Store: stateStore,
		Options: Options{Apply: true, Delete: true},
	}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Gmail rejected trash request") {
		t.Fatalf("error = %v, want trash failure", err)
	}
	if _, ok := stateStore.state.Messages["e1"]; !ok {
		t.Fatal("trash failure removed the source-to-Gmail mapping")
	}
	if stateStore.state.EmailState != "s1" {
		t.Fatalf("source cursor = %q, want s1", stateStore.state.EmailState)
	}
	if len(destination.trashed) != 0 {
		t.Fatalf("failed trash request recorded as successful: %v", destination.trashed)
	}
}

type fakeStore struct {
	state *store.State
	saves int
}

func (f *fakeStore) Load() (*store.State, error) {
	// Most engine fixtures represent an established incremental cache.
	if f.state.EmailState != "" && f.state.GmailHistoryID == 0 {
		f.state.GmailHistoryID = 1
	}
	if f.state.EmailState != "" && f.state.MailboxScope == "" {
		f.state.MailboxScope = mailboxScopeFingerprint(nil, false)
	}
	return f.state, nil
}
func (f *fakeStore) Save(state *store.State) error {
	f.state = state
	f.saves++
	return nil
}

func standardLabels() []Label {
	result := standardLabelsWithoutMarker()
	result = append(result, Label{ID: testManagedLabel(), Name: testManagedLabel(), Type: "user"})
	return result
}

func standardLabelsWithoutMarker() []Label {
	var result []Label
	for _, name := range []string{LabelInbox, LabelSent, LabelDraft, LabelImportant, LabelStarred, LabelUnread, LabelSpam, LabelTrash} {
		result = append(result, Label{ID: name, Name: name, Type: "system"})
	}
	return result
}

func testAccountHash() string {
	digest := sha256.Sum256([]byte("test-user|test-account"))
	return hex.EncodeToString(digest[:])
}

func testManagedLabel() string { return "jmapsync-managed/" + testAccountHash() }

func TestInitialSyncInsertsAndPersistsCursor(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}, {ID: "trash", Name: "Trash", Role: "trash"}},
		state:     "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {ID: "e1", BlobID: "b1", MailboxIDs: map[string]bool{"inbox": true}}},
		raw:    map[string]string{"b1": "From: a@example.com\r\n\r\nhello"},
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	stateStore := &fakeStore{state: store.New()}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 1 || result.LabelsCreated != 1 {
		t.Fatalf("result = %#v", result)
	}
	if got, want := destination.inserted, [][]string{{LabelInbox, LabelUnread, testManagedLabel()}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("insert labels = %#v, want %#v", got, want)
	}
	if stateStore.state.EmailState != "s1" || stateStore.state.Messages["e1"].GmailID == "" {
		t.Fatalf("state not committed: %#v", stateStore.state)
	}
}

func TestMissingMessageIDGetsDeterministicSearchableHeader(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}},
		state:     "s1", ids: []string{"source-1"},
		emails: map[string]Email{"source-1": {ID: "source-1", BlobID: "b1", MailboxIDs: map[string]bool{"inbox": true}}},
		raw:    map[string]string{"b1": "From: a@example.com\r\n\r\nhello"},
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	stateStore := &fakeStore{state: store.New()}
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	messageID := effectiveMessageID(source.emails["source-1"])
	if !strings.Contains(destination.insertedRaw[0], "Message-ID: <"+messageID+">\r\n") {
		t.Fatalf("generated message-id %q not injected into %q", messageID, destination.insertedRaw[0])
	}
}

func TestAdoptionReconcilesManagedLabels(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {ID: "e1", BlobID: "b1", MessageIDs: []string{"<m1@example.com>"}, MailboxIDs: map[string]bool{"inbox": true}}},
	}
	candidate := Message{ID: "g1", LabelIDs: []string{LabelTrash}}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": candidate},
		byMessageID: map[string][]Message{"m1@example.com": {candidate}},
	}
	stateStore := &fakeStore{state: store.New()}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Adopted != 1 || result.Modified != 1 || len(destination.inserted) != 0 {
		t.Fatalf("result = %#v, inserted = %#v", result, destination.inserted)
	}
	want := modifyCall{id: "g1", add: []string{LabelInbox, LabelUnread, testManagedLabel()}, remove: []string{LabelTrash}}
	if !reflect.DeepEqual(destination.modified[0], want) {
		t.Fatalf("modify = %#v, want %#v", destination.modified[0], want)
	}
}

func TestFullSyncTrashesAllNonSpamExtras(t *testing.T) {
	source := &fakeSource{mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s2", emails: map[string]Email{}}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g-old": {ID: "g-old"}, "unrelated": {ID: "unrelated"}}}
	state := store.New()
	state.Messages["gone"] = store.Message{GmailID: "g-old"}
	stateStore := &fakeStore{state: state}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Trashed != 2 || !reflect.DeepEqual(destination.trashed, []string{"g-old", "unrelated"}) {
		t.Fatalf("result = %#v, trashed = %#v", result, destination.trashed)
	}
	if !hasLabelID(destination.messages["unrelated"], LabelTrash) {
		t.Fatal("unrelated Gmail extra not trashed")
	}
	if !hasLabelID(destination.messages["g-old"], LabelTrash) {
		t.Fatal("mapped Gmail message was not moved to Trash")
	}
}

func TestDeletionDisabledKeepsTombstoneUntilEnabled(t *testing.T) {
	source := &fakeSource{
		state:   "s2",
		changes: []Changes{{NewState: "s2", Destroyed: []string{"e1"}}},
		emails:  map[string]Email{},
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1"}}}
	state := store.New()
	state.EmailState = "s1"
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}

	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedTrash != 1 || !stateStore.state.Messages["e1"].SourceDeleted {
		t.Fatalf("result = %#v, state = %#v", result, stateStore.state.Messages["e1"])
	}
	if len(destination.trashed) != 0 {
		t.Fatalf("trashed with deletion propagation disabled: %v", destination.trashed)
	}

	source.changes = nil
	result, err = (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Trashed != 1 || !reflect.DeepEqual(destination.trashed, []string{"g1"}) {
		t.Fatalf("result = %#v, trashed = %v", result, destination.trashed)
	}
	if _, ok := stateStore.state.Messages["e1"]; ok {
		t.Fatal("processed tombstone remains in state")
	}
}

func TestNativeGmailSpamIsNotAdopted(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}},
		state:     "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {
			ID: "e1", BlobID: "b1", MessageIDs: []string{"m1@example.com"},
			MailboxIDs: map[string]bool{"inbox": true},
		}},
	}
	spam := Message{ID: "spam", LabelIDs: []string{LabelSpam}}
	mirror := Message{ID: "mirror", LabelIDs: []string{LabelInbox, LabelUnread}}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"spam": spam, "mirror": mirror},
		byMessageID: map[string][]Message{"m1@example.com": {spam, mirror}},
	}
	stateStore := &fakeStore{state: store.New()}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.IgnoredSpam != 1 || result.Adopted != 1 || len(destination.inserted) != 0 {
		t.Fatalf("result = %#v, inserted = %v", result, destination.inserted)
	}
	if got := stateStore.state.Messages["e1"].GmailID; got != "mirror" {
		t.Fatalf("adopted Gmail ID = %q, want mirror", got)
	}
}

func TestMappedNativeGmailSpamIsNotModified(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s2",
		changes: []Changes{{NewState: "s2", Updated: []string{"e1"}}},
		emails: map[string]Email{"e1": {
			ID: "e1", BlobID: "b1", MailboxIDs: map[string]bool{"inbox": true},
		}},
	}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1", LabelIDs: []string{LabelSpam}}},
	}
	state := store.New()
	state.EmailState = "s1"
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.IgnoredSpam != 1 || len(destination.modified) != 0 || len(destination.inserted) != 0 {
		t.Fatalf("result = %#v, modified = %v, inserted = %v", result, destination.modified, destination.inserted)
	}
}

func TestDestroyedNativeGmailSpamIsNotTrashed(t *testing.T) {
	source := &fakeSource{
		state: "s2", changes: []Changes{{NewState: "s2", Destroyed: []string{"e1"}}},
		emails: map[string]Email{},
	}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1", LabelIDs: []string{LabelSpam}}},
	}
	state := store.New()
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.EmailState = "s1"
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.IgnoredSpam != 1 || result.Trashed != 0 || len(destination.trashed) != 0 {
		t.Fatalf("result = %#v, trashed = %v", result, destination.trashed)
	}
	if _, ok := stateStore.state.Messages["e1"]; ok {
		t.Fatal("ignored spam message remains mapped")
	}
	if !hasLabelID(destination.messages["g1"], LabelSpam) {
		t.Fatal("Gmail spam message changed")
	}
}

func TestAmbiguousMessageIDFailsBeforeRebuildMutation(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}},
		state:     "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {
			ID: "e1", BlobID: "b1", MessageIDs: []string{"m1@example.com"},
			MailboxIDs: map[string]bool{"inbox": true},
		}},
		raw: map[string]string{"b1": "Message-ID: <m1@example.com>\r\n\r\nhello"},
	}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{},
		byMessageID: map[string][]Message{"m1@example.com": {{ID: "g1"}, {ID: "g2"}}},
	}
	stateStore := &fakeStore{state: store.New()}
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatalf("error=%v", err)
	}
	if stateStore.saves != 0 || len(destination.inserted)+len(destination.modified)+len(destination.created)+len(destination.trashed) != 0 {
		t.Fatal("ambiguous rebuild mutated data")
	}
}

func TestDryRunDoesNotMutateDestinationOrState(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "custom", Name: "Custom"}}, state: "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {ID: "e1", BlobID: "b1", MailboxIDs: map[string]bool{"custom": true}}},
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	stateStore := &fakeStore{state: store.New()}
	var output bytes.Buffer
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Output: &output}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.LabelsCreated != 1 || result.Inserted != 1 {
		t.Fatalf("result = %#v", result)
	}
	if stateStore.saves != 0 || len(destination.created) != 0 || len(destination.inserted) != 0 {
		t.Fatalf("dry-run mutated data: saves=%d created=%v inserted=%v", stateStore.saves, destination.created, destination.inserted)
	}
}

func TestIncrementalDryRunActionsAreSorted(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}},
		state:     "s2",
		changes: []Changes{{
			NewState:  "s2",
			Destroyed: []string{"z", "a", "z"},
			Created:   []string{"d", "b"},
			Updated:   []string{"c", "b"},
		}},
		emails: map[string]Email{
			"b": {ID: "b", BlobID: "blob-b", MailboxIDs: map[string]bool{"inbox": true}},
			"c": {ID: "c", BlobID: "blob-c", MailboxIDs: map[string]bool{"inbox": true}},
			"d": {ID: "d", BlobID: "blob-d", MailboxIDs: map[string]bool{"inbox": true}},
		},
	}
	destination := &fakeDestination{
		labels: standardLabels(),
		messages: map[string]Message{
			"gmail-a": {ID: "gmail-a"},
			"gmail-z": {ID: "gmail-z"},
		},
		byMessageID: map[string][]Message{},
	}
	state := store.New()
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.EmailState = "s1"
	state.Messages["a"] = store.Message{GmailID: "gmail-a"}
	state.Messages["z"] = store.Message{GmailID: "gmail-z"}
	var output bytes.Buffer

	_, err := (&Engine{
		Source: source, Destination: destination, Store: &fakeStore{state: state},
		Options: Options{Delete: true, Output: &output},
	}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	labels := fmt.Sprintf("[\"INBOX\" \"UNREAD\" \"%s\"]", testManagedLabel())
	want := "trash Gmail message \"gmail-a\" for destroyed Fastmail email \"a\"\n" +
		"trash Gmail message \"gmail-z\" for destroyed Fastmail email \"z\"\n" +
		fmt.Sprintf("insert Fastmail email \"b\" into Gmail with labels %s\n", labels) +
		fmt.Sprintf("insert Fastmail email \"c\" into Gmail with labels %s\n", labels) +
		fmt.Sprintf("insert Fastmail email \"d\" into Gmail with labels %s\n", labels) +
		"leave Gmail and state unchanged\n"
	if got := output.String(); got != want {
		t.Fatalf("action output:\n%s\nwant:\n%s", got, want)
	}
}

func TestIncrementalHistoryExpiryFallsBackToFull(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s2",
		emails: map[string]Email{}, changeErr: ErrCannotCalculateChanges,
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
	state := store.New()
	state.EmailState = "expired"
	stateStore := &fakeStore{state: state}
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stateStore.state.EmailState != "s2" {
		t.Fatalf("state cursor = %q, want s2", stateStore.state.EmailState)
	}
}

func TestGmailHistoryRepairsDestinationLabelDrift(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s2",
		emails: map[string]Email{"e1": {ID: "e1", BlobID: "b1", MailboxIDs: map[string]bool{"inbox": true}}},
	}
	base := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1", LabelIDs: []string{LabelTrash}}}}
	destination := &fakeHistoryDestination{fakeDestination: base, currentHistory: 20, historyIDs: []string{"g1"}}
	state := store.New()
	state.EmailState = "s1"
	state.GmailHistoryID = 10
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Modified != 1 || stateStore.state.GmailHistoryID != 20 {
		t.Fatalf("result = %#v, state = %#v", result, stateStore.state)
	}
	want := modifyCall{id: "g1", add: []string{LabelInbox, LabelUnread, testManagedLabel()}, remove: []string{LabelTrash}}
	if !reflect.DeepEqual(base.modified[0], want) {
		t.Fatalf("modify = %#v, want %#v", base.modified[0], want)
	}
}

func TestGmailHistoryDryRunActionsAreSortedBySourceID(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}},
		state:     "s2",
		emails: map[string]Email{
			"a": {ID: "a", BlobID: "blob-a", MailboxIDs: map[string]bool{"inbox": true}},
			"b": {ID: "b", BlobID: "blob-b", MailboxIDs: map[string]bool{"inbox": true}},
		},
	}
	base := &fakeDestination{
		labels: standardLabels(),
		messages: map[string]Message{
			"gmail-a": {ID: "gmail-a", LabelIDs: []string{testManagedLabel()}},
			"gmail-b": {ID: "gmail-b", LabelIDs: []string{testManagedLabel()}},
		},
	}
	destination := &fakeHistoryDestination{
		fakeDestination: base,
		currentHistory:  20,
		historyIDs:      []string{"gmail-b", "gmail-a", "gmail-b"},
	}
	state := store.New()
	state.EmailState = "s1"
	state.GmailHistoryID = 10
	state.Messages["a"] = store.Message{GmailID: "gmail-a"}
	state.Messages["b"] = store.Message{GmailID: "gmail-b"}
	var output bytes.Buffer

	_, err := (&Engine{
		Source: source, Destination: destination, Store: &fakeStore{state: state},
		Options: Options{Output: &output},
	}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := "modify Gmail message \"gmail-a\" labels (add=[\"INBOX\" \"UNREAD\"] remove=[])\n" +
		"modify Gmail message \"gmail-b\" labels (add=[\"INBOX\" \"UNREAD\"] remove=[])\n" +
		"leave Gmail and state unchanged\n"
	if got := output.String(); got != want {
		t.Fatalf("action output:\n%s\nwant:\n%s", got, want)
	}
}

func TestGmailHistoryReinsertsDeletedMappedMessage(t *testing.T) {
	source := &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s2",
		emails: map[string]Email{"e1": {
			ID: "e1", BlobID: "b1", MessageIDs: []string{"m1@example.com"},
			MailboxIDs: map[string]bool{"inbox": true},
		}},
		raw: map[string]string{"b1": "Message-ID: <m1@example.com>\r\n\r\nhello"},
	}
	base := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{},
		byMessageID: map[string][]Message{},
	}
	destination := &fakeHistoryDestination{fakeDestination: base, currentHistory: 20, historyIDs: []string{"g1"}}
	state := store.New()
	state.EmailState = "s1"
	state.GmailHistoryID = 10
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}
	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 1 || len(base.inserted) != 1 {
		t.Fatalf("result = %#v, inserted = %v", result, base.inserted)
	}
	if got := stateStore.state.Messages["e1"].GmailID; got == "" || got == "g1" {
		t.Fatalf("replacement Gmail ID = %q", got)
	}
}

func TestRememberSkipsUnchangedStateWrite(t *testing.T) {
	email := Email{ID: "e1", BlobID: "b1", MessageIDs: []string{"m1@example.com"}}
	state := store.New()
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	stateStore := &fakeStore{state: state}
	engine := &Engine{Store: stateStore, Options: Options{Apply: true}, state: state}
	if err := engine.remember(email, "g1", []string{LabelInbox}); err != nil {
		t.Fatal(err)
	}
	if stateStore.saves != 0 {
		t.Fatalf("unchanged record caused %d state writes", stateStore.saves)
	}
}

func TestEngineRequiresDependencies(t *testing.T) {
	_, err := (&Engine{}).Run(t.Context())
	if err == nil {
		t.Fatal("expected dependency error")
	}
}

func TestAccountBindingFailsBeforeMutation(t *testing.T) {
	source := &identifiedSource{fakeSource: &fakeSource{}, identity: "new-account"}
	destination := &fakeDestination{}
	state := store.New()
	state.SourceAccount = "old-account"
	stateStore := &fakeStore{state: state}
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "state belongs to another Fastmail account") {
		t.Fatalf("error = %v, want Fastmail account mismatch", err)
	}
	if stateStore.saves != 0 || len(destination.trashed) != 0 {
		t.Fatalf("account mismatch mutated state or destination: saves=%d trashed=%v", stateStore.saves, destination.trashed)
	}
}

func TestMissingCacheRebuildsManagedMapping(t *testing.T) {
	source := oneMessageSource()
	managed := Message{
		ID: "g1", LabelIDs: []string{LabelInbox, LabelUnread, testManagedLabel()},
		AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{"e1"},
		MessageIDHeaders: []string{"<m1@example.com>"},
	}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": managed}, byMessageID: map[string][]Message{}}
	state := store.New()
	state.Rebuild = true
	state.RebuildReason = "is missing"
	stateStore := &fakeStore{state: state}

	result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 0 || len(destination.inserted) != 0 || stateStore.state.Messages["e1"].GmailID != "g1" {
		t.Fatalf("result = %#v, inserted = %v, state = %#v", result, destination.inserted, stateStore.state.Messages)
	}
}

func TestRepeatedCachelessRunsInsertOnce(t *testing.T) {
	source := oneMessageSource()
	destination := &fakeDestination{labels: standardLabelsWithoutMarker(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	for range 2 {
		_, err := (&Engine{Source: source, Destination: destination, Store: store.NewMemory(), Options: Options{Apply: true, Delete: true}}).Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(destination.inserted) != 1 {
		t.Fatalf("insert count = %d, want one", len(destination.inserted))
	}
}

func TestIncrementalInsertIsRecoveredAfterMappingSaveFails(t *testing.T) {
	source := oneMessageSource()
	source.changes = []Changes{{NewState: "s1", Created: []string{"e1"}}}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	path := filepath.Join(t.TempDir(), "state.json")
	initial := store.New()
	initial.SourceAccount = testAccountHash()
	initial.GmailAccount = "test-gmail@example.com"
	initial.EmailState = "s0"
	initial.Mailboxes["inbox"] = store.Mailbox{GmailLabelID: LabelInbox, Name: LabelInbox}
	if err := (store.File{Path: path}).Save(initial); err != nil {
		t.Fatal(err)
	}
	failing := &failNthSaveStore{file: store.File{Path: path}, failAt: 3}
	_, err := (&Engine{Source: source, Destination: destination, Store: failing, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "simulated mapping save failure") {
		t.Fatalf("error = %v, want mapping save failure", err)
	}
	if failing.saves != 3 {
		t.Fatalf("save attempts = %d, want three", failing.saves)
	}
	if len(destination.inserted) != 1 {
		t.Fatalf("first run inserted %d messages, want one", len(destination.inserted))
	}
	inserted := destination.messages["inserted-1"]
	if !hasLabelID(inserted, testManagedLabel()) ||
		!reflect.DeepEqual(inserted.AccountHeaders, []string{testAccountHash()}) ||
		!reflect.DeepEqual(inserted.EmailIDHeaders, []string{"e1"}) {
		t.Fatalf("inserted message lacks recovery identity: %#v", inserted)
	}
	persisted, err := (store.File{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Incomplete || !persisted.Rebuild || persisted.RebuildReason != "was not committed" {
		t.Fatalf("persisted state = %#v, want incomplete rebuild", persisted)
	}
	if _, exists := persisted.Messages["e1"]; exists {
		t.Fatalf("failed mapping save persisted mapping: %#v", persisted.Messages)
	}

	if _, err := (&Engine{Source: source, Destination: destination, Store: store.File{Path: path}, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered, err := (store.File{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(destination.inserted) != 1 || recovered.Messages["e1"].GmailID != "inserted-1" {
		t.Fatalf("inserted = %d, recovered = %#v", len(destination.inserted), recovered.Messages)
	}
}

func TestIncompleteCheckpointFailurePreventsGmailMutation(t *testing.T) {
	source := oneMessageSource()
	source.mailboxes = append(source.mailboxes, Mailbox{ID: "new", Name: "Create Me"})
	source.changes = []Changes{{NewState: "s1", Created: []string{"e1"}}}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	path := filepath.Join(t.TempDir(), "state.json")
	initial := store.New()
	initial.SourceAccount = testAccountHash()
	initial.GmailAccount = "test-gmail@example.com"
	initial.EmailState = "s0"
	if err := (store.File{Path: path}).Save(initial); err != nil {
		t.Fatal(err)
	}
	failing := &failNthSaveStore{file: store.File{Path: path}, failAt: 1}

	_, err := (&Engine{Source: source, Destination: destination, Store: failing, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "simulated mapping save failure") {
		t.Fatalf("error = %v, want checkpoint failure", err)
	}
	if failing.saves != 1 {
		t.Fatalf("save attempts = %d, want one", failing.saves)
	}
	if len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatalf("checkpoint failure mutated Gmail: %#v", destination)
	}
	loaded, err := (store.File{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Rebuild || loaded.Incomplete || loaded.EmailState != "s0" {
		t.Fatalf("persisted state changed after failed checkpoint: %#v", loaded)
	}
}

func TestInsertRewritesIdentityHeadersAndPreservesBody(t *testing.T) {
	source := oneMessageSource()
	body := "body\x00bytes\r\nunchanged"
	source.raw["b1"] = "x-jmapsync-account: attacker\r\n\tfolded\r\nX-JMAPSYNC-EMAIL-ID: wrong\r\n more\r\nSubject: keep\r\n\r\n" + body
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	if _, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: store.New()}, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw := destination.insertedRaw[0]
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 || parts[1] != body {
		t.Fatalf("body changed: %q", raw)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := fakeHeaderValues(parsed.Header, HeaderAccount); !reflect.DeepEqual(got, []string{testAccountHash()}) {
		t.Fatalf("account headers = %#v", got)
	}
	if got := fakeHeaderValues(parsed.Header, HeaderEmailID); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Fatalf("email ID headers = %#v", got)
	}
}

func TestUnmarkedIdentityHeadersAreNotAdopted(t *testing.T) {
	candidate := Message{ID: "g1", AccountHeaders: []string{"spoofed"}, EmailIDHeaders: []string{"e1"}}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": candidate},
		byMessageID: map[string][]Message{"m1@example.com": {candidate}},
	}
	result, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: &fakeStore{state: rebuildState("is missing")}, Options: Options{Apply: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Adopted != 0 || result.Inserted != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestMarkerLabelIsRestored(t *testing.T) {
	source := oneMessageSource()
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"g1": {ID: "g1", LabelIDs: []string{LabelInbox, LabelUnread}}},
	}
	state := store.New()
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	if _, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Full: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !hasLabelID(destination.messages["g1"], testManagedLabel()) {
		t.Fatalf("marker label not restored: %#v", destination.messages["g1"])
	}
}

func TestInvalidRebuildInventoryFailsBeforeMutation(t *testing.T) {
	tests := map[string][]Message{
		"partial":   {{ID: "g1", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{testAccountHash()}}},
		"empty":     {{ID: "g1", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{""}}},
		"duplicate": {{ID: "g1", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{testAccountHash(), testAccountHash()}, EmailIDHeaders: []string{"e1"}}},
		"wrong":     {{ID: "g1", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{"wrong"}, EmailIDHeaders: []string{"e1"}}},
		"ambiguous legacy": {
			{ID: "g1", LabelIDs: []string{testManagedLabel()}, MessageIDHeaders: []string{"m1@example.com"}},
			{ID: "g2", LabelIDs: []string{testManagedLabel()}, MessageIDHeaders: []string{"m1@example.com"}},
		},
		"duplicate claims": {
			{ID: "g1", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{"e1"}},
			{ID: "g2", LabelIDs: []string{testManagedLabel()}, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{"e1"}},
		},
	}
	for name, inventory := range tests {
		t.Run(name, func(t *testing.T) {
			destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
			for _, message := range inventory {
				destination.messages[message.ID] = message
			}
			_, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: &fakeStore{state: rebuildState("is corrupt")}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
			if err == nil {
				t.Fatal("invalid inventory succeeded")
			}
			if len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
				t.Fatalf("preflight failure mutated Gmail: %#v", destination)
			}
		})
	}
}

func TestAmbiguousInitialAdoptionFailsBeforeLabelMutation(t *testing.T) {
	source := oneMessageSource()
	source.mailboxes = append(source.mailboxes, Mailbox{ID: "custom", Name: "Create Me"})
	destination := &fakeDestination{
		labels:      standardLabelsWithoutMarker(),
		messages:    map[string]Message{},
		byMessageID: map[string][]Message{"m1@example.com": {{ID: "g1"}, {ID: "g2"}}},
	}
	_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: rebuildState("is missing")}, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatalf("error = %v", err)
	}
	if len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatalf("ambiguous preflight mutated Gmail: %#v", destination)
	}
}

func TestLaterMessageIDPageMakesPreflightAmbiguous(t *testing.T) {
	source := oneMessageSource()
	source.mailboxes = append(source.mailboxes, Mailbox{ID: "custom", Name: "Create Me"})
	destination := &fakeDestination{
		labels: standardLabelsWithoutMarker(), messages: map[string]Message{},
		messageIDPages: map[string][][]Message{"m1@example.com": {{{ID: "g1"}}, {{ID: "g2"}}}},
	}
	stateStore := &fakeStore{state: rebuildState("is missing")}

	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatalf("error = %v, want ambiguous adoption", err)
	}
	if stateStore.saves != 0 || len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatalf("later-page ambiguity mutated state or Gmail: saves=%d destination=%#v", stateStore.saves, destination)
	}
}

func TestSharedSourceMessageIDFailsSafeDuringRebuild(t *testing.T) {
	source := oneMessageSource()
	source.ids = append(source.ids, "e2")
	source.emails["e2"] = Email{ID: "e2", BlobID: "b2", MessageIDs: []string{"m1@example.com"}, MailboxIDs: map[string]bool{"inbox": true}}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{},
		byMessageID: map[string][]Message{"m1@example.com": {{ID: "g1"}}},
	}
	_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: rebuildState("is missing")}, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatalf("error = %v", err)
	}
	if len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
		t.Fatalf("ambiguous source IDs mutated Gmail: %#v", destination)
	}
}

func TestCorruptCacheIsNotOverwrittenWhenPreflightFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	original := []byte("{broken")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{stateErr: errors.New("Fastmail unavailable")}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
	_, err := (&Engine{Source: source, Destination: destination, Store: store.File{Path: path}, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Fastmail unavailable") {
		t.Fatalf("error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("corrupt cache was overwritten: %q", got)
	}
	if len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
		t.Fatalf("failed preflight mutated Gmail: %#v", destination)
	}
}

func TestRebuildTrashesManagedAndUnmanagedExtras(t *testing.T) {
	orphan := Message{
		ID: "orphan", LabelIDs: []string{testManagedLabel()},
		AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{"gone"},
	}
	spam := Message{ID: "spam", LabelIDs: []string{testManagedLabel(), LabelSpam}, AccountHeaders: []string{"bad"}}
	destination := &fakeDestination{
		labels: standardLabels(), messages: map[string]Message{"orphan": orphan, "spam": spam, "unrelated": {ID: "unrelated"}},
		byMessageID: map[string][]Message{},
	}
	source := &fakeSource{state: "s1", emails: map[string]Email{}}
	result, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: rebuildState("is missing")}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Trashed != 2 || !reflect.DeepEqual(destination.trashed, []string{"orphan", "unrelated"}) || !hasLabelID(destination.messages["spam"], LabelSpam) {
		t.Fatalf("result = %#v, trashed = %v, spam = %#v", result, destination.trashed, destination.messages["spam"])
	}
}

func TestFailedFinalCommitLeavesRebuildSignal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}
	failing := &failFinalStore{file: store.File{Path: path}}
	_, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: failing, Options: Options{Apply: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "simulated final commit failure") {
		t.Fatalf("error = %v", err)
	}
	loaded, err := (store.File{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Rebuild || !loaded.Incomplete {
		t.Fatalf("loaded state = %#v, want incomplete rebuild", loaded)
	}
	if _, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: store.File{Path: path}, Options: Options{Apply: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.inserted) != 1 {
		t.Fatalf("recovery inserted %d messages, want one total", len(destination.inserted))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"incomplete": true`) {
		t.Fatalf("successful recovery left incomplete state: %s", data)
	}
}

func TestMalformedMarkedSpamIsIgnoredDuringRebuild(t *testing.T) {
	spam := Message{ID: "spam", LabelIDs: []string{testManagedLabel(), LabelSpam}, AccountHeaders: []string{"bad", "worse"}}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"spam": spam}, byMessageID: map[string][]Message{}}
	result, err := (&Engine{Source: &fakeSource{state: "s1", emails: map[string]Email{}}, Destination: destination, Store: &fakeStore{state: rebuildState("is missing")}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.IgnoredSpam != 1 || len(destination.modified) != 0 || len(destination.trashed) != 0 {
		t.Fatalf("result = %#v, modified = %v, trashed = %v", result, destination.modified, destination.trashed)
	}
}

func TestDryRunReportsExactActionsWithoutMutation(t *testing.T) {
	source := oneMessageSource()
	source.mailboxes = append(source.mailboxes, Mailbox{ID: "new", Name: "New Label"}, Mailbox{ID: "create", Name: "Create Label"})
	spam := Message{ID: "spam", LabelIDs: []string{LabelSpam}}
	candidate := Message{ID: "g1", LabelIDs: []string{LabelTrash}}
	destination := &fakeDestination{
		labels:      append(standardLabels(), Label{ID: "old", Name: "Old Label", Type: "user"}),
		messages:    map[string]Message{"g1": candidate, "gone": {ID: "gone"}, "spam": spam},
		byMessageID: map[string][]Message{"m1@example.com": {spam, candidate}},
	}
	state := store.New()
	state.Mailboxes["new"] = store.Mailbox{GmailLabelID: "old", Name: "Old Label"}
	state.Messages["gone-source"] = store.Message{GmailID: "gone"}
	stateStore := &fakeStore{state: state}
	var output bytes.Buffer
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Full: true, Delete: true, Output: &output}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`create Gmail label "Create Label"`,
		`rename Gmail label "Old Label" to "New Label"`,
		`adopt Gmail message "g1" for Fastmail email "e1"`,
		`modify Gmail message "g1" labels (add=["INBOX" "UNREAD" "jmapsync-managed/`,
		`trash Gmail message "gone" for destroyed Fastmail email "gone-source"`,
		`ignore marked Gmail spam message "spam"`,
		`leave Gmail and state unchanged`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, output.String())
		}
	}
	if stateStore.saves != 0 || len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatalf("dry run mutated state or Gmail: saves=%d destination=%#v", stateStore.saves, destination)
	}
}

func oneMessageSource() *fakeSource {
	return &fakeSource{
		mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, state: "s1", ids: []string{"e1"},
		emails: map[string]Email{"e1": {
			ID: "e1", BlobID: "b1", MessageIDs: []string{"m1@example.com"},
			MailboxIDs: map[string]bool{"inbox": true},
		}},
		raw: map[string]string{"b1": "Message-ID: <m1@example.com>\r\n\r\nhello"},
	}
}

func rebuildState(reason string) *store.State {
	state := store.New()
	state.Rebuild = true
	state.RebuildReason = reason
	return state
}

type failNthSaveStore struct {
	file   store.File
	failAt int
	saves  int
}

func (f *failNthSaveStore) Load() (*store.State, error) { return f.file.Load() }

func (f *failNthSaveStore) Save(state *store.State) error {
	f.saves++
	if f.saves == f.failAt {
		return errors.New("simulated mapping save failure")
	}
	return f.file.Save(state)
}

type failFinalStore struct {
	file store.File
}

func (f *failFinalStore) Load() (*store.State, error) { return f.file.Load() }

func (f *failFinalStore) Save(state *store.State) error {
	if !state.Incomplete {
		return errors.New("simulated final commit failure")
	}
	return f.file.Save(state)
}
