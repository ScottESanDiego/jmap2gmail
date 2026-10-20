package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"jmap2gmail/internal/store"
)

type reuseDestination struct {
	*fakeDestination
	gets        []string
	inventories int
	modifyError error
	beforeGet   func(string)
}

func (d *reuseDestination) Inventory(ctx context.Context) ([]Message, error) {
	d.inventories++
	return d.fakeDestination.Inventory(ctx)
}
func (d *reuseDestination) Message(ctx context.Context, id string) (Message, error) {
	d.gets = append(d.gets, id)
	if d.beforeGet != nil {
		d.beforeGet(id)
	}
	return d.fakeDestination.Message(ctx, id)
}
func (d *reuseDestination) ModifyLabels(ctx context.Context, id string, add, remove []string) (Message, error) {
	if d.modifyError != nil {
		return Message{}, d.modifyError
	}
	return d.fakeDestination.ModifyLabels(ctx, id, add, remove)
}

type reuseSource struct {
	*fakeSource
	beforeChanges func()
}

func (s *reuseSource) Changes(ctx context.Context, since string) (Changes, error) {
	if s.beforeChanges != nil {
		s.beforeChanges()
	}
	return s.fakeSource.Changes(ctx, since)
}

func markedInventoryMessage(gmailID, sourceID string, labels []string) Message {
	return Message{ID: gmailID, LabelIDs: labels, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{sourceID}}
}

func TestFullReconciliationReusesMappedAdoptedAndSpamInventory(t *testing.T) {
	for _, kind := range []string{"mapped", "adopted", "spam"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/apply=%t", kind, apply), func(t *testing.T) {
				source := oneMessageSource()
				state := store.New()
				message := markedInventoryMessage("g1", "e1", []string{LabelInbox, LabelUnread, testManagedLabel()})
				if kind == "adopted" {
					message = Message{ID: "g1", LabelIDs: []string{LabelInbox, LabelUnread}, MessageIDHeaders: []string{"m1@example.com"}}
				} else {
					state.Messages["e1"] = store.Message{GmailID: "g1"}
				}
				if kind == "spam" {
					message.LabelIDs = []string{LabelSpam, testManagedLabel()}
				}
				destination := &reuseDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": message}}}
				stateStore := &fakeStore{state: state}
				engine := &Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: apply, Full: true}}
				result, err := engine.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(destination.gets) != 0 || destination.inventories != 1 {
					t.Fatalf("redundant destination reads: gets=%v inventory=%d", destination.gets, destination.inventories)
				}
				if engine.reconcileInventory != nil || engine.inventoryValidated {
					t.Fatal("inventory reuse survived initial reconciliation")
				}
				switch kind {
				case "mapped":
					if result.Unchanged != 1 {
						t.Fatalf("mapped result=%#v", result)
					}
				case "adopted":
					if result.Adopted != 1 || result.Modified != 1 {
						t.Fatalf("adoption result=%#v", result)
					}
				case "spam":
					if result.IgnoredSpam != 1 {
						t.Fatalf("spam result=%#v", result)
					}
				}
				if !apply && (stateStore.saves != 0 || len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0) {
					t.Fatal("dry inventory reuse mutated Gmail/state")
				}
			})
		}
	}
}

func TestFullCatchupDeletionAndCleanupUseFreshMessages(t *testing.T) {
	base := oneMessageSource()
	base.changes = []Changes{{NewState: "s2", Updated: []string{"e1"}}}
	state := store.New()
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	state.Messages["gone"] = store.Message{GmailID: "old"}
	destination := &reuseDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{
		"g1":  markedInventoryMessage("g1", "e1", []string{LabelInbox, LabelUnread, testManagedLabel()}),
		"old": {ID: "old", LabelIDs: []string{LabelInbox}}, "extra": {ID: "extra", LabelIDs: []string{LabelInbox}},
	}}}
	changed := false
	source := &reuseSource{fakeSource: base, beforeChanges: func() {
		if !changed {
			message := destination.messages["g1"]
			message.LabelIDs = []string{LabelInbox, testManagedLabel()}
			destination.messages["g1"] = message
			changed = true
		}
	}}
	engine := &Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Delete: true, Full: true}}
	destination.beforeGet = func(id string) {
		if engine.reconcileInventory != nil {
			t.Fatalf("stale inventory available during fresh read %q", id)
		}
	}
	result, err := engine.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(destination.gets, []string{"old", "g1", "extra"}) {
		t.Fatalf("fresh destination reads=%v", destination.gets)
	}
	if result.Unchanged != 1 || result.Modified != 1 || result.Trashed != 2 || !hasLabelID(destination.messages["g1"], LabelUnread) {
		t.Fatalf("fresh catchup result=%#v message=%#v", result, destination.messages["g1"])
	}
}

func TestIncrementalReconciliationAlwaysGetsFreshMessages(t *testing.T) {
	source := oneMessageSource()
	source.changes = []Changes{{NewState: "s2", Updated: []string{"e1"}}}
	state := store.New()
	state.EmailState = "s1"
	state.GmailHistoryID = 1
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	destination := &reuseDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": markedInventoryMessage("g1", "e1", []string{LabelInbox, testManagedLabel()})}}}
	engine := &Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true}}
	// Even an engine reused by a caller must not retain the previous run's reuse map.
	engine.reconcileInventory = map[string]Message{"g1": markedInventoryMessage("g1", "e1", []string{LabelInbox, LabelUnread, testManagedLabel()})}
	engine.inventoryValidated = true
	result, err := engine.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if destination.inventories != 0 || !reflect.DeepEqual(destination.gets, []string{"g1"}) || result.Modified != 1 {
		t.Fatalf("inventories=%d gets=%v result=%#v", destination.inventories, destination.gets, result)
	}
}

func TestStaleInventoryModifyNotFoundFailsWithoutReinsert(t *testing.T) {
	source := oneMessageSource()
	destination := &reuseDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{"g1": markedInventoryMessage("g1", "e1", []string{LabelInbox, testManagedLabel()})}}, modifyError: ErrNotFound}
	state := store.New()
	state.Messages["e1"] = store.Message{GmailID: "g1"}
	var output bytes.Buffer
	engine := &Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Full: true, Output: &output}}
	if _, err := engine.Run(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale modification error=%v", err)
	}
	if len(destination.inserted) != 0 || len(destination.gets) != 0 || engine.reconcileInventory != nil || engine.inventoryValidated {
		t.Fatalf("stale modification replayed or retained cache: gets=%v inserts=%v", destination.gets, destination.inserted)
	}
	if strings.Contains(output.String(), "reconcile: full Fastmail snapshot (1/1") {
		t.Fatalf("failed email reported complete:\n%s", output.String())
	}
	if state.EmailState != "" {
		t.Fatal("failed modification advanced source cursor")
	}
}

func fullProgressLines(output string) []string {
	var result []string
	for line := range strings.Lines(output) {
		if strings.HasPrefix(line, "reconcile: full Fastmail snapshot") {
			result = append(result, strings.TrimSuffix(line, "\n"))
		}
	}
	return result
}

func unchangedProgressFixture(total int) (*fakeSource, *reuseDestination, *store.State) {
	source := &fakeSource{state: "s1", mailboxes: []Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, emails: map[string]Email{}}
	destination := &reuseDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}}
	state := store.New()
	for i := range total {
		id := fmt.Sprintf("e%03d", i)
		gid := fmt.Sprintf("g%03d", i)
		source.ids = append(source.ids, id)
		source.emails[id] = Email{ID: id, MailboxIDs: map[string]bool{"inbox": true}, Keywords: map[string]bool{"$seen": true}}
		destination.messages[gid] = markedInventoryMessage(gid, id, []string{LabelInbox, testManagedLabel()})
		state.Messages[id] = store.Message{GmailID: gid}
	}
	return source, destination, state
}

func TestFullUnchangedReconciliationProgressMilestones(t *testing.T) {
	for _, tc := range []struct {
		name    string
		total   int
		advance map[int]time.Duration
		want    []string
	}{
		{name: "empty", total: 0, want: []string{"0/0 messages, unchanged 0, elapsed 0s"}},
		{name: "first and final", total: 3, want: []string{"0/3 messages, unchanged 0, elapsed 0s", "1/3 messages, unchanged 1, elapsed 0s", "3/3 messages, unchanged 3, elapsed 0s"}},
		{name: "hundreds", total: 201, want: []string{"0/201 messages, unchanged 0, elapsed 0s", "1/201 messages, unchanged 1, elapsed 0s", "100/201 messages, unchanged 100, elapsed 0s", "200/201 messages, unchanged 200, elapsed 0s", "201/201 messages, unchanged 201, elapsed 0s"}},
		{name: "final is milestone", total: 100, want: []string{"0/100 messages, unchanged 0, elapsed 0s", "1/100 messages, unchanged 1, elapsed 0s", "100/100 messages, unchanged 100, elapsed 0s"}},
		{name: "time since prior report", total: 6, advance: map[int]time.Duration{1: 2 * time.Second, 2: 6 * time.Second, 3: 7 * time.Second, 4: 11 * time.Second, 5: 12 * time.Second}, want: []string{"0/6 messages, unchanged 0, elapsed 0s", "1/6 messages, unchanged 1, elapsed 2s", "3/6 messages, unchanged 3, elapsed 7s", "5/6 messages, unchanged 5, elapsed 12s", "6/6 messages, unchanged 6, elapsed 12s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, destination, state := unchangedProgressFixture(tc.total)
			var output bytes.Buffer
			started := time.Unix(1000, 0)
			now := started
			clockCalls := 0
			engine := &Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Full: true, Output: &output}, now: func() time.Time {
				if strings.Contains(output.String(), "reconcile: full Fastmail snapshot (0/") {
					clockCalls++
					if elapsed, ok := tc.advance[clockCalls]; ok {
						now = started.Add(elapsed)
					}
				}
				return now
			}}
			result, err := engine.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, line := range tc.want {
				want = append(want, "reconcile: full Fastmail snapshot ("+line+")")
			}
			if got := fullProgressLines(output.String()); !reflect.DeepEqual(got, want) {
				t.Fatalf("progress=%q want=%q", got, want)
			}
			if result.Unchanged != tc.total || len(destination.gets) != 0 {
				t.Fatalf("unchanged=%d gets=%v", result.Unchanged, destination.gets)
			}
		})
	}
}

func TestReconciliationProgressDoesNotCompleteFailedEmail(t *testing.T) {
	source, destination, state := unchangedProgressFixture(3)
	message := destination.messages["g001"]
	message.LabelIDs = []string{testManagedLabel()}
	destination.messages["g001"] = message
	destination.modifyError = ErrNotFound
	var output bytes.Buffer
	engine := &Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: true, Full: true, Output: &output}, now: func() time.Time { return time.Unix(0, 0) }}
	if _, err := engine.Run(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error=%v", err)
	}
	want := []string{"reconcile: full Fastmail snapshot (0/3 messages, unchanged 0, elapsed 0s)", "reconcile: full Fastmail snapshot (1/3 messages, unchanged 1, elapsed 0s)"}
	if got := fullProgressLines(output.String()); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed progress=%q want=%q", got, want)
	}
	if engine.result.Unchanged != 1 || engine.reconcileInventory != nil {
		t.Fatalf("failure result=%#v cache=%v", engine.result, engine.reconcileInventory)
	}
}
