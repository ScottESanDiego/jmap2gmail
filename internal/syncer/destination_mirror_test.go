package syncer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"jmap2gmail/internal/store"
)

type mirrorDestination struct {
	*fakeDestination
	inventoryErr      error
	inventoryOverride []Message
	inventoryCalls    int
	findCalls         int
	beforeInventory   func()
	historyIDs        []string
	historyErr        error
	historyStarts     []uint64
	baselineRead      bool
}

func (d *mirrorDestination) Inventory(ctx context.Context) ([]Message, error) {
	d.inventoryCalls++
	if d.beforeInventory != nil {
		d.beforeInventory()
	}
	if d.inventoryErr != nil {
		return nil, d.inventoryErr
	}
	if d.inventoryOverride != nil {
		return d.inventoryOverride, nil
	}
	return d.fakeDestination.Inventory(ctx)
}
func (d *mirrorDestination) FindByMessageID(ctx context.Context, id string) ([]Message, error) {
	d.findCalls++
	return d.fakeDestination.FindByMessageID(ctx, id)
}
func (d *mirrorDestination) CurrentHistoryID(context.Context) (uint64, error) {
	d.baselineRead = true
	return 50, nil
}
func (d *mirrorDestination) ChangesSince(_ context.Context, start uint64) ([]string, error) {
	d.historyStarts = append(d.historyStarts, start)
	return d.historyIDs, d.historyErr
}

func TestMirrorExtrasFollowDeletePolicyAndAreIdempotent(t *testing.T) {
	for _, apply := range []bool{false, true} {
		for _, deleteEnabled := range []bool{false, true} {
			t.Run(map[bool]string{false: "dry", true: "apply"}[apply]+map[bool]string{false: "-retain", true: "-trash"}[deleteEnabled], func(t *testing.T) {
				source := oneMessageSource()
				matched := Message{ID: "matched", LabelIDs: []string{LabelTrash}, MessageIDHeaders: []string{"m1@example.com"}}
				destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{
					"matched": matched, "extra": {ID: "extra"}, "trashed": {ID: "trashed", LabelIDs: []string{LabelTrash}}, "spam": {ID: "spam", LabelIDs: []string{LabelSpam}},
				}}}
				stateStore := &fakeStore{state: store.New()}
				result, err := (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: apply, Delete: deleteEnabled}}).Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if destination.findCalls != 0 || destination.inventoryCalls != 1 {
					t.Fatalf("full matching used searches/repeated inventory: searches=%d inventory=%d", destination.findCalls, destination.inventoryCalls)
				}
				if result.Adopted != 1 || result.Modified != 1 {
					t.Fatalf("matching result=%#v", result)
				}
				if deleteEnabled && result.Trashed != 1 || !deleteEnabled && result.Trashed != 0 {
					t.Fatalf("extra count=%#v", result)
				}
				if apply {
					if hasLabelID(destination.messages["matched"], LabelTrash) {
						t.Fatal("matching copy not restored")
					}
					if hasLabelID(destination.messages["extra"], LabelTrash) != deleteEnabled {
						t.Fatal("extra policy ignored")
					}
				} else if stateStore.saves != 0 || len(destination.modified)+len(destination.trashed) != 0 {
					t.Fatal("dry run mutated data")
				}
				if hasLabelID(destination.messages["spam"], LabelTrash) {
					t.Fatal("native spam trashed")
				}
				if apply && deleteEnabled {
					result, err = (&Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: true}}).Run(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if result.Trashed != 0 || len(destination.trashed) != 1 {
						t.Fatalf("already-Trash extra repeated: result=%#v calls=%v", result, destination.trashed)
					}
				}
			})
		}
	}
}

func TestGmailInventoryFailureAndMalformedIDsPreventAllWrites(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		messages []Message
	}{
		{"failure", errors.New("inventory interrupted"), nil},
		{"empty ID", nil, []Message{{ID: ""}}},
		{"duplicate ID", nil, []Message{{ID: "dup"}, {ID: "dup"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabelsWithoutMarker(), messages: map[string]Message{"extra": {ID: "extra"}}}, inventoryErr: test.err, inventoryOverride: test.messages}
			state := store.New()
			stateStore := &fakeStore{state: state}
			_, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
			if err == nil {
				t.Fatal("incomplete inventory accepted")
			}
			t.Logf("preflight rejected inventory: %v", err)
			if stateStore.saves != 0 || len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
				t.Fatal("invalid inventory caused a mutation")
			}
		})
	}
}

func TestUnknownGmailHistoryTriggersCompleteTruth(t *testing.T) {
	for _, test := range []struct {
		name          string
		message       Message
		exists        bool
		wantInventory int
	}{
		{"new extra", Message{ID: "added"}, true, 1},
		{"spam", Message{ID: "added", LabelIDs: []string{LabelSpam}}, true, 0},
		{"already Trash", Message{ID: "added", LabelIDs: []string{LabelTrash}}, true, 0},
		{"permanently gone", Message{}, false, 0},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: "-dry", true: "-apply"}[apply], func(t *testing.T) {
				source := oneMessageSource()
				state := store.New()
				state.MailboxScope = mailboxScopeFingerprint(nil, true)
				state.EmailState = "s1"
				state.GmailHistoryID = 10
				state.Messages["e1"] = store.Message{GmailID: "matched"}
				messages := map[string]Message{"matched": scopedMessage("matched", "e1", LabelInbox, LabelUnread)}
				if test.exists {
					messages["added"] = test.message
				}
				destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: messages}, historyIDs: []string{"added", "added"}}
				result, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: state}, Options: Options{Apply: apply, Delete: true}}).Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if destination.inventoryCalls != test.wantInventory {
					t.Fatalf("inventory calls=%d want %d", destination.inventoryCalls, test.wantInventory)
				}
				if test.wantInventory == 1 && result.Trashed != 1 {
					t.Fatalf("extra not counted once: %#v", result)
				}
				if apply && state.GmailHistoryID != 50 {
					t.Fatalf("saved cursor=%d", state.GmailHistoryID)
				}
			})
		}
	}
}

func TestGmailHistoryGapUsesFullInventoryAndEarlyBaseline(t *testing.T) {
	source := oneMessageSource()
	state := store.New()
	state.MailboxScope = mailboxScopeFingerprint(nil, true)
	state.EmailState = "s1"
	state.GmailHistoryID = 10
	destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{"extra": {ID: "extra"}}}}
	destination.beforeInventory = func() {
		if !destination.baselineRead {
			t.Fatal("Gmail baseline captured after inventory")
		}
	}
	destination.historyErr = ErrCannotCalculateDestinationChanges
	// Only the old persisted cursor is expired. The baseline captured before
	// inventory remains usable for catch-up.
	wrapped := &gapDestination{mirrorDestination: destination}
	result, err := (&Engine{Source: source, Destination: wrapped, Store: &fakeStore{state: state}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Trashed != 1 || destination.inventoryCalls != 1 || !reflect.DeepEqual(destination.historyStarts, []uint64{10, 50}) {
		t.Fatalf("gap plan=%#v inventories=%d starts=%v", result, destination.inventoryCalls, destination.historyStarts)
	}
}

type gapDestination struct{ *mirrorDestination }

func (d *gapDestination) ChangesSince(_ context.Context, start uint64) ([]string, error) {
	d.historyStarts = append(d.historyStarts, start)
	if start == 10 {
		return nil, ErrCannotCalculateDestinationChanges
	}
	return nil, nil
}

func TestAmbiguousLocalInventoryMatchingPreventsExtraDeletion(t *testing.T) {
	destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabelsWithoutMarker(), messages: map[string]Message{
		"a": {ID: "a", MessageIDHeaders: []string{"m1@example.com"}}, "b": {ID: "b", MessageIDHeaders: []string{"m1@example.com"}}, "extra": {ID: "extra"},
	}}}
	stateStore := &fakeStore{state: store.New()}
	_, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatalf("error=%v", err)
	}
	if stateStore.saves != 0 || len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
		t.Fatal("ambiguous inventory mutated data")
	}
}

func TestInventoryRecoversCurrentAccountIdentityWithoutMarker(t *testing.T) {
	for _, markerLabelExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "marker label missing", true: "message marker missing"}[markerLabelExists], func(t *testing.T) {
			labels := standardLabelsWithoutMarker()
			if markerLabelExists {
				labels = standardLabels()
			}
			destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: labels, messages: map[string]Message{
				"original": {ID: "original", LabelIDs: []string{LabelTrash}, AccountHeaders: []string{testAccountHash()}, EmailIDHeaders: []string{"e1"}},
			}}}
			stateStore := &fakeStore{state: store.New()}
			result, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Inserted != 0 || result.Trashed != 0 || stateStore.state.Messages["e1"].GmailID != "original" || hasLabelID(destination.messages["original"], LabelTrash) || !hasLabelID(destination.messages["original"], map[bool]string{false: "label-" + testManagedLabel(), true: testManagedLabel()}[markerLabelExists]) {
				t.Fatalf("identity not restored: result=%#v state=%#v message=%#v", result, stateStore.state.Messages, destination.messages["original"])
			}
		})
	}
}

func TestLegacyCacheAndDeletePolicyChangesForceFullMirror(t *testing.T) {
	source := oneMessageSource()
	destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{
		"matched": scopedMessage("matched", "e1", LabelInbox, LabelUnread), "extra": {ID: "extra"},
	}}}
	file := store.File{Path: t.TempDir() + "/state.json"}
	legacy := store.New()
	legacy.SourceAccount = testAccountHash()
	legacy.GmailAccount = "test-gmail@example.com"
	legacy.EmailState = "s1"
	legacy.GmailHistoryID = 10
	legacy.Messages["e1"] = store.Message{GmailID: "matched"}
	legacy.Mailboxes["inbox"] = store.Mailbox{GmailLabelID: LabelInbox, Name: LabelInbox}
	if err := file.Save(legacy); err != nil {
		t.Fatal(err)
	}
	for index, deleteEnabled := range []bool{false, true} {
		result, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: deleteEnabled}}).Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if destination.inventoryCalls != index+1 {
			t.Fatalf("policy change skipped complete inventory: %d", destination.inventoryCalls)
		}
		if hasLabelID(destination.messages["extra"], LabelTrash) != deleteEnabled {
			t.Fatalf("delete policy %t did not reconcile extra", deleteEnabled)
		}
		if deleteEnabled && result.Trashed != 1 {
			t.Fatalf("delete toggle result=%#v", result)
		}
		loaded, err := file.Load()
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Messages["e1"].GmailID != "matched" || loaded.Mailboxes["inbox"].GmailLabelID != LabelInbox || loaded.SourceAccount != testAccountHash() || loaded.MailboxScope != mailboxScopeFingerprint(nil, deleteEnabled) {
			t.Fatalf("policy migration lost identity/history: %#v", loaded)
		}
	}
}

func TestEarlyGmailBaselineCatchesAdditionDuringInventory(t *testing.T) {
	destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{
		"matched": scopedMessage("matched", "e1", LabelInbox, LabelUnread), "late": {ID: "late"},
	}}, historyIDs: []string{"late"}}
	destination.inventoryOverride = []Message{destination.messages["matched"]}
	destination.beforeInventory = func() {
		if destination.inventoryCalls > 1 {
			destination.inventoryOverride = nil
		}
	}
	source := &progressSource{fakeSource: oneMessageSource(), before: func(operation string) {
		if !destination.baselineRead {
			t.Fatalf("Fastmail %s began before Gmail baseline", operation)
		}
	}}
	result, err := (&Engine{Source: source, Destination: destination, Store: &fakeStore{state: store.New()}, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if destination.inventoryCalls != 2 || result.Trashed != 1 || !reflect.DeepEqual(destination.trashed, []string{"late"}) {
		t.Fatalf("late addition not reconciled once: inventories=%d result=%#v trash=%v", destination.inventoryCalls, result, destination.trashed)
	}
	for _, start := range destination.historyStarts {
		if start != 50 {
			t.Fatalf("catch-up used late baseline %d", start)
		}
	}
}

func TestCompleteInventoryReplacesOnlyAbsentCachedIdentityClaim(t *testing.T) {
	for _, oldStillPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale old ID", true: "live duplicate claim"}[oldStillPresent], func(t *testing.T) {
			destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{
				"replacement": scopedMessage("replacement", "e1", LabelInbox, LabelUnread),
			}}}
			if oldStillPresent {
				destination.messages["old"] = scopedMessage("old", "e1", LabelInbox, LabelUnread)
			}
			state := store.New()
			state.EmailState = "s1"
			state.GmailHistoryID = 10
			state.MailboxScope = mailboxScopeFingerprint(nil, true)
			state.Messages["e1"] = store.Message{GmailID: "old"}
			stateStore := &fakeStore{state: state}
			result, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: true}}).Run(t.Context())
			if oldStillPresent {
				if err == nil || !strings.Contains(err.Error(), "duplicate Gmail claims") {
					t.Fatalf("error=%v", err)
				}
				if stateStore.saves != 0 || len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
					t.Fatal("duplicate claim mutated data")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.Inserted != 0 || result.Trashed != 0 || state.Messages["e1"].GmailID != "replacement" {
					t.Fatalf("stale cache not repaired: result=%#v mapping=%#v", result, state.Messages["e1"])
				}
			}
		})
	}
}

func TestStaleCachedMappingUsesLocalAdoptionPreflight(t *testing.T) {
	for _, candidateCount := range []int{1, 2} {
		t.Run(map[int]string{1: "single local match", 2: "ambiguous local matches"}[candidateCount], func(t *testing.T) {
			destination := &mirrorDestination{fakeDestination: &fakeDestination{labels: standardLabelsWithoutMarker(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}}
			for _, id := range []string{"candidate-a", "candidate-b"}[:candidateCount] {
				message := Message{ID: id, MessageIDHeaders: []string{"m1@example.com"}}
				destination.messages[id] = message
				destination.byMessageID["m1@example.com"] = append(destination.byMessageID["m1@example.com"], message)
			}
			state := store.New()
			state.EmailState = "s1"
			state.GmailHistoryID = 10
			state.MailboxScope = mailboxScopeFingerprint(nil, true)
			state.Messages["e1"] = store.Message{GmailID: "missing-old-id"}
			stateStore := &fakeStore{state: state}
			result, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Delete: true, Full: true}}).Run(t.Context())
			if candidateCount == 2 {
				if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
					t.Fatalf("error=%v want preflight ambiguity", err)
				}
				if stateStore.saves != 0 || len(destination.created)+len(destination.modified)+len(destination.inserted)+len(destination.trashed) != 0 {
					t.Fatal("ambiguous stale-cache matching mutated data")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.Adopted != 1 || result.Inserted != 0 || state.Messages["e1"].GmailID != "candidate-a" {
					t.Fatalf("local adoption=%#v mapping=%#v", result, state.Messages["e1"])
				}
			}
			if destination.findCalls != 0 {
				t.Fatalf("full preflight fell back to network searches: %d", destination.findCalls)
			}
		})
	}
}
