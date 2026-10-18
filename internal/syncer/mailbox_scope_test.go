package syncer

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jmapsync/internal/store"
)

type scopedSource struct {
	*fakeSource
	queries    [][]string
	allQueries int
}

func (s *scopedSource) EmailIDsInMailboxes(_ context.Context, ids []string) ([]string, error) {
	s.queries = append(s.queries, slices.Clone(ids))
	var result []string
	for id, email := range s.emails {
		for _, mailbox := range ids {
			if email.MailboxIDs[mailbox] {
				result = append(result, id)
				break
			}
		}
	}
	return sortedUniqueStrings(result), nil
}
func (s *scopedSource) AllEmailIDs(ctx context.Context) ([]string, error) {
	s.allQueries++
	return s.fakeSource.AllEmailIDs(ctx)
}

func testScopedSource() *scopedSource {
	return &scopedSource{fakeSource: &fakeSource{
		mailboxes: []Mailbox{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "child", Name: "Child", ParentID: "a"}},
		state:     "s2", ids: []string{"e1", "e2", "e3"},
		emails: map[string]Email{
			"e1": {ID: "e1", BlobID: "blob1", MailboxIDs: map[string]bool{"a": true, "b": true}},
			"e2": {ID: "e2", BlobID: "blob2", MailboxIDs: map[string]bool{"b": true}},
			"e3": {ID: "e3", BlobID: "blob3", MailboxIDs: map[string]bool{"child": true}},
		},
		raw: map[string]string{"blob1": "Subject: one\r\n\r\none", "blob2": "Subject: two\r\n\r\ntwo", "blob3": "Subject: three\r\n\r\nthree"},
	}}
}
func scopeKey(ids ...string) string {
	return mailboxScopeFingerprint(sortedUniqueStrings(ids), false)
}
func scopedState(ids ...string) *store.State {
	s := store.New()
	s.EmailState = "s1"
	s.MailboxScope = scopeKey(ids...)
	return s
}
func scopeLabels() []Label {
	return append(standardLabels(), Label{ID: "label-a", Name: "A", Type: "user"}, Label{ID: "label-b", Name: "B", Type: "user"}, Label{ID: "personal", Name: "Personal", Type: "user"})
}
func scopedMessage(id, sourceID string, labels ...string) Message {
	return Message{ID: id, LabelIDs: append(labels, testManagedLabel()), AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{sourceID}}
}

func TestResolveMailboxIDsExactPaths(t *testing.T) {
	mailboxes := testScopedSource().mailboxes
	for _, test := range []struct{ paths, want []string }{
		{nil, nil}, {[]string{}, nil}, {[]string{"A"}, []string{"a"}},
		{[]string{"A/Child", "A", "A"}, []string{"a", "child"}},
		{[]string{"B", "A"}, []string{"a", "b"}},
	} {
		got, err := ResolveMailboxIDs(mailboxes, test.paths)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("ResolveMailboxIDs(%v)=%v,%v want %v", test.paths, got, err, test.want)
		}
	}
	for _, path := range []string{"a", "Child", "A/child", " A", "A/"} {
		if _, err := ResolveMailboxIDs(mailboxes, []string{path}); err == nil {
			t.Fatalf("unknown path %q accepted", path)
		}
	}
	for name, boxes := range map[string][]Mailbox{
		"ambiguous":      {{ID: "1", Name: "A"}, {ID: "2", Name: "A"}},
		"cycle":          {{ID: "1", Name: "A", ParentID: "2"}, {ID: "2", Name: "B", ParentID: "1"}},
		"missing parent": {{ID: "1", Name: "A", ParentID: "missing"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveMailboxIDs(boxes, []string{"A"}); err == nil {
				t.Fatal("invalid hierarchy accepted")
			}
		})
	}
}

func TestFilteredUnknownPathFailsBeforeWrites(t *testing.T) {
	source := testScopedSource()
	destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{}}
	stateStore := &fakeStore{state: store.New()}
	_, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, IncludeMailboxes: []string{"Typo"}}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "unknown mailbox path") {
		t.Fatalf("error=%v", err)
	}
	if stateStore.saves != 0 || len(destination.created)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatal("invalid selection wrote state or Gmail")
	}
}

func TestFilteredViewAcrossModes(t *testing.T) {
	for _, mode := range []string{"cached-full", "cached-incremental", "cacheless"} {
		for _, apply := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-dry", true: "-apply"}[apply], func(t *testing.T) {
				source := testScopedSource()
				if mode == "cached-incremental" {
					source.changes = []Changes{{NewState: "s2", Updated: []string{"e3", "e2", "e1"}}}
				}
				destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"g1": scopedMessage("g1", "e1", "label-b", "personal")}}
				state := scopedState("a")
				state.Messages["e1"] = store.Message{GmailID: "g1"}
				state.Mailboxes["b"] = store.Mailbox{GmailLabelID: "label-b", Name: "B"}
				stateStore := &fakeStore{state: state}
				var backend StateStore = stateStore
				if mode == "cacheless" {
					backend = store.NewMemory()
				}
				var output bytes.Buffer
				result, err := (&Engine{Source: source, Destination: destination, Store: backend, Options: Options{Apply: apply, Full: mode == "cached-full", IncludeMailboxes: []string{"A"}, Output: &output}}).Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if result.Inserted != 0 || result.Modified != 1 || result.Trashed != 0 {
					t.Fatalf("result=%#v output=%s", result, output.String())
				}
				if strings.Contains(output.String(), `email "e2"`) || strings.Contains(output.String(), `email "e3"`) {
					t.Fatalf("out-of-scope operation: %s", output.String())
				}
				if source.allQueries != 0 {
					t.Fatal("filtered run used account-wide inventory")
				}
				if mode != "cached-incremental" && !reflect.DeepEqual(source.queries, [][]string{{"a"}}) {
					t.Fatalf("queries=%v", source.queries)
				}
				if apply {
					if !hasLabelID(destination.messages["g1"], "label-b") || !hasLabelID(destination.messages["g1"], "personal") || !hasLabelID(destination.messages["g1"], "label-a") {
						t.Fatalf("unrelated/selected labels=%v", destination.messages["g1"].LabelIDs)
					}
				} else if stateStore.saves != 0 || len(destination.modified) != 0 {
					t.Fatal("dry run wrote data")
				}
				if len(destination.created) != 0 {
					t.Fatalf("created excluded labels: %v", destination.created)
				}
			})
		}
	}
}

func TestFilteredMoveOutAndDestructionUseDeletePolicy(t *testing.T) {
	for _, event := range []string{"move", "destroy"} {
		for _, deleteEnabled := range []bool{false, true} {
			t.Run(event+map[bool]string{false: "-retain", true: "-trash"}[deleteEnabled], func(t *testing.T) {
				source := testScopedSource()
				source.emails["e1"] = Email{ID: "e1", MailboxIDs: map[string]bool{"b": true}}
				change := Changes{NewState: "s2", Updated: []string{"e1", "e2"}}
				if event == "destroy" {
					change = Changes{NewState: "s2", Destroyed: []string{"e1", "e2"}}
				}
				source.changes = []Changes{change}
				state := scopedState("a")
				state.MailboxScope = mailboxScopeFingerprint([]string{"a"}, deleteEnabled)
				state.Messages["e1"] = store.Message{GmailID: "g1"}
				destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"g1": scopedMessage("g1", "e1", "label-a"), "unmanaged": {ID: "unmanaged", LabelIDs: []string{"label-a"}}}}
				backend := &fakeStore{state: state}
				_, err := (&Engine{Source: source, Destination: destination, Store: backend, Options: Options{Apply: true, Delete: deleteEnabled, IncludeMailboxes: []string{"A"}}}).Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if hasLabelID(destination.messages["unmanaged"], LabelTrash) {
					t.Fatal("unmanaged mail changed")
				}
				if deleteEnabled {
					if !reflect.DeepEqual(destination.trashed, []string{"g1"}) {
						t.Fatalf("trashed=%v", destination.trashed)
					}
				} else {
					if !state.Messages["e1"].SourceDeleted || len(destination.trashed) != 0 {
						t.Fatalf("retained state=%#v", state.Messages)
					}
					source.changes = nil
					if _, err := (&Engine{Source: source, Destination: destination, Store: backend, Options: Options{Apply: true, Delete: true, IncludeMailboxes: []string{"A"}}}).Run(t.Context()); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(destination.trashed, []string{"g1", "unmanaged"}) {
						t.Fatalf("pending deletion=%v", destination.trashed)
					}
				}
			})
		}
	}
}

func TestFilteredFullAbsenceDeletesMappedCopies(t *testing.T) {
	for _, deleteEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain", true: "trash"}[deleteEnabled], func(t *testing.T) {
			source := testScopedSource()
			source.emails["e1"] = Email{ID: "e1", MailboxIDs: map[string]bool{"b": true}}
			state := scopedState("a")
			state.Messages["e1"] = store.Message{GmailID: "g1"}
			state.Messages["deleted"] = store.Message{GmailID: "g2"}
			destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"g1": scopedMessage("g1", "e1", "label-a"), "g2": scopedMessage("g2", "deleted", "label-b"), "unmanaged": {ID: "unmanaged"}}}
			_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Full: true, Delete: deleteEnabled, IncludeMailboxes: []string{"A"}}}).Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if hasLabelID(destination.messages["unmanaged"], LabelTrash) != deleteEnabled {
				t.Fatal("extra cleanup ignored delete policy")
			}
			if deleteEnabled && !reflect.DeepEqual(destination.trashed, []string{"g2", "g1", "unmanaged"}) {
				t.Fatalf("trashed=%v", destination.trashed)
			}
			if !deleteEnabled && (!state.Messages["e1"].SourceDeleted || !state.Messages["deleted"].SourceDeleted) {
				t.Fatal("absent view members not tombstoned")
			}
		})
	}
}

func TestReorderedSelectionPreservesCursor(t *testing.T) {
	source := testScopedSource()
	source.changes = []Changes{{NewState: "s2", Destroyed: []string{"gone"}}}
	state := scopedState("a", "b")
	state.MailboxScope = mailboxScopeFingerprint([]string{"a", "b"}, true)
	state.Messages["gone"] = store.Message{GmailID: "g"}
	destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"g": scopedMessage("g", "gone", "label-a")}}
	_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Delete: true, IncludeMailboxes: []string{"B", "A", "B"}}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(source.queries) != 0 || !reflect.DeepEqual(destination.trashed, []string{"g"}) {
		t.Fatalf("reordered selection rebuilt: queries=%v trash=%v", source.queries, destination.trashed)
	}
}

func TestFilteredCachelessRecoveryRemovesAllExcludedManagedCopies(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "dry", true: "apply"}[apply], func(t *testing.T) {
			source := testScopedSource()
			source.emails = map[string]Email{}
			destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{
				"prior-a": scopedMessage("prior-a", "left-a", "label-a"), "prior-b": scopedMessage("prior-b", "left-b", "label-b"), "marker-only": scopedMessage("marker-only", "unknown"), "unmanaged": {ID: "unmanaged"},
			}}
			result, err := (&Engine{Source: source, Destination: destination, Store: store.NewMemory(), Options: Options{Apply: apply, Delete: true, IncludeMailboxes: []string{"A"}}}).Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Trashed != 4 {
				t.Fatalf("result=%#v", result)
			}
			if apply && !reflect.DeepEqual(destination.trashed, []string{"prior-a", "prior-b", "marker-only", "unmanaged"}) {
				t.Fatalf("trash=%v", destination.trashed)
			}
			if !apply && len(destination.trashed) != 0 {
				t.Fatal("dry run wrote Gmail")
			}
			if hasLabelID(destination.messages["unmanaged"], LabelTrash) != apply {
				t.Fatal("extra cleanup did not match dry/apply")
			}
		})
	}
}

func TestMoveBetweenSelectedMailboxesUpdatesLabels(t *testing.T) {
	source := testScopedSource()
	source.emails["e1"] = Email{ID: "e1", MailboxIDs: map[string]bool{"b": true}}
	source.changes = []Changes{{NewState: "s2", Updated: []string{"e1"}}}
	state := scopedState("a", "b")
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"g1": scopedMessage("g1", "e1", "label-a", LabelUnread)}}
	_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Delete: true, IncludeMailboxes: []string{"A", "B"}}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(destination.trashed) != 0 || len(destination.modified) != 1 || !hasLabelID(destination.messages["g1"], "label-b") || hasLabelID(destination.messages["g1"], "label-a") {
		t.Fatalf("selected move=%#v", destination)
	}
}

func TestFilteredMoveOutAndReturnReuseGmailCopy(t *testing.T) {
	for _, mode := range []string{"incremental", "full", "cacheless"} {
		t.Run(mode, func(t *testing.T) {
			source := testScopedSource()
			source.emails["e1"] = Email{ID: "e1", BlobID: "blob1", MailboxIDs: map[string]bool{"a": true}}
			destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{}}
			backend := &fakeStore{state: store.New()}
			run := func() {
				t.Helper()
				var stateStore StateStore = backend
				if mode == "cacheless" {
					stateStore = store.NewMemory()
				}
				if _, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: mode == "full", IncludeMailboxes: []string{"A"}}}).Run(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			run()
			if len(destination.inserted) != 1 {
				t.Fatalf("first insertions=%v", destination.inserted)
			}
			id := "inserted-1"
			email := source.emails["e1"]
			email.MailboxIDs = map[string]bool{"b": true}
			source.emails["e1"] = email
			source.changes = []Changes{{NewState: "s3", Updated: []string{"e1"}}}
			source.state = "s3"
			run()
			if !hasLabelID(destination.messages[id], LabelTrash) {
				t.Fatal("move out did not Trash")
			}
			run()
			if len(destination.trashed) != 1 {
				t.Fatalf("repeated excluded sync retried Trash: %v", destination.trashed)
			}
			email.MailboxIDs = map[string]bool{"a": true}
			source.emails["e1"] = email
			source.changes = []Changes{{NewState: "s4", Updated: []string{"e1"}}}
			source.state = "s4"
			run()
			if len(destination.inserted) != 1 || hasLabelID(destination.messages[id], LabelTrash) {
				t.Fatalf("return duplicated or failed restoration: inserted=%v message=%#v", destination.inserted, destination.messages[id])
			}
			if mode != "cacheless" && (backend.state.Messages["e1"].GmailID != id || backend.state.Messages["e1"].SourceDeleted) {
				t.Fatalf("restored mapping=%#v", backend.state.Messages["e1"])
			}
		})
	}
}

func TestSelectionChangesReconcileViewAndReuseMappings(t *testing.T) {
	for _, mode := range []string{"cached", "full", "cacheless"} {
		t.Run(mode, func(t *testing.T) {
			source := testScopedSource()
			source.emails["e1"] = Email{ID: "e1", BlobID: "blob1", MailboxIDs: map[string]bool{"a": true}}
			destination := &fakeDestination{labels: scopeLabels(), messages: map[string]Message{"unmanaged": {ID: "unmanaged"}}}
			backend := &fakeStore{state: store.New()}
			for index, selection := range [][]string{{"A"}, {"B"}, nil} {
				var stateStore StateStore = backend
				if mode == "cacheless" {
					stateStore = store.NewMemory()
				}
				if _, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: mode == "full", IncludeMailboxes: selection}}).Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if index == 1 && !hasLabelID(destination.messages["inserted-1"], LabelTrash) {
					t.Fatal("config narrowing did not remove prior managed copy")
				}
				if index == 2 && (hasLabelID(destination.messages["inserted-1"], LabelTrash) || hasLabelID(destination.messages["inserted-2"], LabelTrash)) {
					t.Fatal("filtered-to-all did not restore original copies")
				}
			}
			if len(destination.inserted) != 3 {
				t.Fatalf("scope swaps duplicated messages: count=%d", len(destination.inserted))
			}
			if !hasLabelID(destination.messages["unmanaged"], LabelTrash) {
				t.Fatal("unmanaged extra retained")
			}
			if mode != "cacheless" && (backend.state.MailboxScope != mailboxScopeFingerprint(nil, true) || backend.state.Messages["e1"].GmailID != "inserted-1" || backend.state.Messages["e2"].GmailID != "inserted-2") {
				t.Fatalf("scope/mappings=%#v", backend.state)
			}
		})
	}
}
