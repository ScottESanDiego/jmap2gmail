package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jmapsync/internal/store"
)

type fetchSource struct {
	*fakeSource
	fetch   func([]string) ([]Email, error)
	batches [][]string
}

func (f *fetchSource) Emails(_ context.Context, ids []string) ([]Email, error) {
	f.batches = append(f.batches, append([]string(nil), ids...))
	return f.fetch(ids)
}

func TestInvalidSnapshotMetadataFailsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		ids    []string
		emails []Email
		want   string
	}{
		{"substituted ID with same count", []string{"e1"}, []Email{{ID: "other"}}, "unrequested email"},
		{"missing metadata", []string{"e1"}, nil, "snapshot was incomplete"},
		{"duplicate inventory", []string{"e1", "e1"}, []Email{{ID: "e1"}}, "snapshot contains duplicate IDs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := oneMessageSource()
			base.ids = test.ids
			source := &fetchSource{fakeSource: base, fetch: func([]string) ([]Email, error) { return test.emails, nil }}
			destination := &fakeDestination{labels: standardLabelsWithoutMarker(), messages: map[string]Message{}}
			stateStore := &fakeStore{state: rebuildState("is missing")}
			engine := &Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}
			_, err := engine.Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if stateStore.saves != 0 || len(destination.created)+len(destination.renamed)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
				t.Fatalf("invalid snapshot mutated Gmail or state: saves=%d destination=%#v", stateStore.saves, destination)
			}
			if engine.snapshot != nil || engine.snapshotIDs != nil || engine.snapshotAt != "" {
				t.Fatal("failed snapshot published partial metadata")
			}
		})
	}
}

func TestMalformedIncrementalMetadataPreservesCursor(t *testing.T) {
	for _, test := range []struct {
		name   string
		emails []Email
		want   string
	}{
		{"empty", []Email{{ID: "e1", BlobID: "b1"}, {}}, "empty ID"},
		{"duplicate", []Email{{ID: "e1", BlobID: "b1"}, {ID: "e1"}}, "duplicate email"},
		{"unrequested", []Email{{ID: "e1", BlobID: "b1"}, {ID: "other"}}, "unrequested email"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := oneMessageSource()
			base.changes = []Changes{{NewState: "s2", Updated: []string{"e1", "e2"}}}
			source := &fetchSource{fakeSource: base, fetch: func([]string) ([]Email, error) { return test.emails, nil }}
			state := store.New()
			state.EmailState = "s1"
			destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
			_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true}}).Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			t.Logf("rejected malformed source response: %v", err)
			if state.EmailState != "s1" || !state.Incomplete {
				t.Fatalf("failed batch advanced cursor or cleared checkpoint: %#v", state)
			}
			if len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
				t.Fatalf("malformed batch processed before validation: %#v", destination)
			}
		})
	}
}

func TestSourceFetchBatchesPreserveSortedActions(t *testing.T) {
	for _, mode := range []string{"full", "incremental", "destination"} {
		t.Run(mode, func(t *testing.T) {
			base := &fakeSource{state: "s2", ids: []string{"e", "a", "d", "c", "b"}}
			source := &fetchSource{fakeSource: base, fetch: func(ids []string) ([]Email, error) {
				var emails []Email
				for _, id := range slices.Backward(ids) {
					emails = append(emails, Email{ID: id})
				}
				return emails, nil
			}}
			state := store.New()
			if mode != "full" {
				state.EmailState = "s1"
			}
			baseDestination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
			var destination Destination = baseDestination
			if mode == "incremental" {
				base.changes = []Changes{{NewState: "s2", Created: []string{"e", "a", "d"}, Updated: []string{"c", "b", "a"}}}
			}
			if mode == "destination" {
				state.GmailHistoryID = 1
				for _, id := range base.ids {
					state.Messages[id] = store.Message{GmailID: "gmail-" + id}
				}
				destination = &fakeHistoryDestination{fakeDestination: baseDestination, currentHistory: 2, historyIDs: []string{"gmail-e", "gmail-a", "gmail-d", "gmail-c", "gmail-b", "gmail-a"}}
			}
			var output bytes.Buffer
			_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{BatchSize: 2, Output: &output}}).Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
			if !reflect.DeepEqual(source.batches, want) {
				t.Fatalf("batches = %v, want %v", source.batches, want)
			}
			previous := -1
			for _, id := range []string{"a", "b", "c", "d", "e"} {
				position := strings.Index(output.String(), fmt.Sprintf("insert Fastmail email %q", id))
				if position <= previous {
					t.Fatalf("actions out of order for %q:\n%s", id, output.String())
				}
				previous = position
			}
		})
	}
}

func TestLaterSourceBatchFailurePreservesCursor(t *testing.T) {
	base := oneMessageSource()
	base.changes = []Changes{{NewState: "s2", Created: []string{"e1", "e2"}}}
	fetchErr := errors.New("second batch unavailable")
	source := &fetchSource{fakeSource: base, fetch: func(ids []string) ([]Email, error) {
		if ids[0] == "e2" {
			return nil, fetchErr
		}
		return []Email{base.emails["e1"]}, nil
	}}
	state := store.New()
	state.EmailState = "s1"
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
	_, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, BatchSize: 1}}).Run(t.Context())
	if !errors.Is(err, fetchErr) {
		t.Fatalf("error = %v, want %v", err, fetchErr)
	}
	if state.EmailState != "s1" || !state.Incomplete {
		t.Fatalf("failed batch advanced cursor or cleared checkpoint: %#v", state)
	}
	if len(destination.inserted) != 1 || state.Messages["e1"].GmailID == "" {
		t.Fatalf("earlier successful batch lost: inserted=%v state=%#v", destination.inserted, state)
	}
}

func TestMissingIncrementalMetadataRetainsMapping(t *testing.T) {
	base := &fakeSource{state: "s2", changes: []Changes{{NewState: "s2", Updated: []string{"gone"}}}}
	state := store.New()
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.EmailState = "s1"
	state.Messages["gone"] = store.Message{GmailID: "gmail-gone"}
	destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{"gmail-gone": {ID: "gmail-gone"}}}
	_, err := (&Engine{Source: base, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.EmailState != "s2" || state.Incomplete {
		t.Fatalf("successful page not committed: %#v", state)
	}
	if state.Messages["gone"].GmailID != "gmail-gone" || state.Messages["gone"].SourceDeleted || len(destination.trashed) != 0 {
		t.Fatalf("missing metadata treated as deletion: state=%#v trashed=%v", state, destination.trashed)
	}
}
