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

	"jmapsync/internal/store"
)

// These hooks inspect output from inside the operations whose latency the
// announcements explain; checking only final output would miss late reports.
type progressSource struct {
	*fakeSource
	before func(string)
	fetch  func([]string) ([]Email, error)
}

func (s *progressSource) State(ctx context.Context) (string, error) {
	s.before("state")
	return s.fakeSource.State(ctx)
}

func (s *progressSource) AllEmailIDs(ctx context.Context) ([]string, error) {
	s.before("inventory")
	return s.fakeSource.AllEmailIDs(ctx)
}

func (s *progressSource) Emails(ctx context.Context, ids []string) ([]Email, error) {
	s.before("metadata")
	if s.fetch != nil {
		return s.fetch(ids)
	}
	return s.fakeSource.Emails(ctx, ids)
}

type progressDestination struct {
	*fakeDestination
	before    func(string)
	find      func(string) ([]Message, error)
	inventory func() ([]Message, error)
}

func (d *progressDestination) Labels(ctx context.Context) ([]Label, error) {
	d.before("labels")
	return d.fakeDestination.Labels(ctx)
}

func (d *progressDestination) Inventory(ctx context.Context) ([]Message, error) {
	d.before("gmailinventory")
	if d.inventory != nil {
		return d.inventory()
	}
	return d.fakeDestination.Inventory(ctx)
}

func (d *progressDestination) ManagedMessages(ctx context.Context, label string) ([]Message, error) {
	d.before("managed")
	return d.fakeDestination.ManagedMessages(ctx, label)
}

func (d *progressDestination) FindByMessageID(ctx context.Context, id string) ([]Message, error) {
	d.before("matching")
	if d.find != nil {
		return d.find(id)
	}
	return d.fakeDestination.FindByMessageID(ctx, id)
}

func rebuildProgressLines(output string) []string {
	var lines []string
	for line := range strings.Lines(output) {
		if strings.HasPrefix(line, "rebuild:") {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

func TestRebuildPhaseAnnouncementsPrecedeOperations(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprintf("marker=%t", marker), func(t *testing.T) {
			var output bytes.Buffer
			var operations []string
			before := func(operation string) {
				operations = append(operations, operation)
				var want string
				switch operation {
				case "state", "inventory":
					want = "rebuild: inventorying Fastmail messages"
				case "metadata":
					want = "rebuild: loading Fastmail metadata (0/1 messages)"
				case "labels", "gmailinventory":
					want = "rebuild: inventorying Gmail metadata"
				case "matching":
					want = "rebuild: matching Gmail messages by Message-ID (0/1 messages)"
				}
				if !strings.Contains(output.String(), want+"\n") {
					t.Fatalf("operation %s started before %q:\n%s", operation, want, output.String())
				}
			}
			labels := standardLabelsWithoutMarker()
			if marker {
				labels = standardLabels()
			}
			destination := &progressDestination{fakeDestination: &fakeDestination{labels: labels, messages: map[string]Message{}}, before: before}
			stateStore := &fakeStore{state: rebuildState("is missing")}
			engine := &Engine{
				Source:      &progressSource{fakeSource: oneMessageSource(), before: before},
				Destination: destination, Store: stateStore, Options: Options{Output: &output},
				now: func() time.Time { return time.Unix(0, 0) },
			}
			if _, err := engine.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			wantOperations := []string{"state", "inventory", "metadata", "labels"}
			wantOperations = append(wantOperations, "gmailinventory")
			if !reflect.DeepEqual(operations, wantOperations) {
				t.Fatalf("operations=%v, want %v", operations, wantOperations)
			}
			want := []string{
				"rebuild: inventorying Fastmail messages",
				"rebuild: loading Fastmail metadata (0/1 messages)",
				"rebuild: loading Fastmail metadata (1/1 messages)",
				"rebuild: inventorying Gmail metadata",
				"rebuild: matching Gmail messages by Message-ID (0/1 messages)",
				"rebuild: matched Gmail messages by Message-ID (1/1 messages, elapsed 0s)",
			}
			if got := rebuildProgressLines(output.String()); !reflect.DeepEqual(got, want) {
				t.Fatalf("progress=%q, want %q", got, want)
			}
			if stateStore.saves != 0 || len(destination.created)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
				t.Fatal("dry-run progress mutated state or Gmail")
			}
		})
	}
}

func TestRebuildMetadataProgressCountsEachCompletedBatch(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		fail  bool
		want  []string
	}{
		{"multiple batches", 5, false, []string{"0/5", "2/5", "4/5", "5/5"}},
		{"empty", 0, false, []string{"0/0"}},
		{"later batch failure", 5, true, []string{"0/5", "2/5"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &fakeSource{state: "new", emails: map[string]Email{}}
			for i := range test.count {
				id := fmt.Sprintf("e%d", i)
				base.ids = append(base.ids, id)
				base.emails[id] = Email{ID: id}
			}
			var output bytes.Buffer
			calls := 0
			failure := errors.New("metadata unavailable")
			source := &progressSource{fakeSource: base, before: func(string) {}, fetch: func(ids []string) ([]Email, error) {
				// The previous batch must already have been reported when the
				// next slow fetch starts.
				want := fmt.Sprintf("rebuild: loading Fastmail metadata (%d/%d messages)\n", calls*2, test.count)
				if !strings.Contains(output.String(), want) {
					t.Fatalf("next batch started before progress %q", want)
				}
				calls++
				if test.fail && calls == 2 {
					return nil, failure
				}
				return base.Emails(t.Context(), ids)
			}}
			state := rebuildState("is missing")
			state.EmailState = "old"
			state.GmailHistoryID = 7
			stateStore := &fakeStore{state: state}
			destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
			engine := &Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: test.fail, BatchSize: 2, Output: &output}}
			_, err := engine.Run(t.Context())
			if test.fail {
				if !errors.Is(err, failure) {
					t.Fatalf("error=%v, want %v", err, failure)
				}
				t.Logf("failed preflight: %v", err)
				if stateStore.saves != 0 || state.EmailState != "old" || state.GmailHistoryID != 7 || state.Incomplete || engine.snapshot != nil {
					t.Fatalf("failed preflight published metadata or state: %#v", state)
				}
				if len(destination.created)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
					t.Fatal("failed preflight mutated Gmail")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, line := range rebuildProgressLines(output.String()) {
				if strings.HasPrefix(line, "rebuild: loading Fastmail metadata") {
					got = append(got, line)
				}
			}
			var want []string
			for _, count := range test.want {
				want = append(want, "rebuild: loading Fastmail metadata ("+count+" messages)")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("progress=%q, want %q", got, want)
			}
		})
	}
}

func TestRebuildMatchingProgressTriggers(t *testing.T) {
	for _, test := range []struct {
		name    string
		total   int
		advance map[int]time.Duration
		want    []string
	}{
		{"empty", 0, nil, []string{"0/0 messages, elapsed 0s"}},
		{"first and final", 3, nil, []string{"1/3 messages, elapsed 0s", "3/3 messages, elapsed 0s"}},
		{"count milestones", 201, nil, []string{"1/201 messages, elapsed 0s", "100/201 messages, elapsed 0s", "200/201 messages, elapsed 0s", "201/201 messages, elapsed 0s"}},
		{"time since previous report", 6, map[int]time.Duration{1: 2 * time.Second, 2: 6 * time.Second, 3: 7 * time.Second, 4: 11 * time.Second, 5: 12 * time.Second}, []string{"1/6 messages, elapsed 2s", "3/6 messages, elapsed 7s", "5/6 messages, elapsed 12s", "6/6 messages, elapsed 12s"}},
		{"final is milestone", 100, nil, []string{"1/100 messages, elapsed 0s", "100/100 messages, elapsed 0s"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			start := time.Unix(1000, 0)
			now := start
			clockCalls := 0
			engine := &Engine{Destination: &fakeDestination{}, state: store.New(), snapshot: map[string]Email{}, Options: Options{Output: &output}, now: func() time.Time {
				if !strings.Contains(output.String(), fmt.Sprintf("rebuild: matching Gmail messages by Message-ID (0/%d messages)\n", test.total)) {
					t.Fatal("local matching started before announcement")
				}
				if elapsed, ok := test.advance[clockCalls]; ok {
					now = start.Add(elapsed)
				}
				clockCalls++
				return now
			}}
			for i := test.total - 1; i >= 0; i-- {
				id := fmt.Sprintf("private-%03d", i)
				engine.snapshot[id] = Email{ID: id, MessageIDs: []string{id}}
			}
			if err := engine.preflightAdoptions(t.Context(), map[string]string{}); err != nil {
				t.Fatal(err)
			}
			want := []string{fmt.Sprintf("rebuild: matching Gmail messages by Message-ID (0/%d messages)", test.total)}
			for _, progress := range test.want {
				want = append(want, "rebuild: matched Gmail messages by Message-ID ("+progress+")")
			}
			if got := rebuildProgressLines(output.String()); !reflect.DeepEqual(got, want) {
				t.Fatalf("progress=%q, want %q", got, want)
			}
			if strings.Contains(output.String(), "private-") {
				t.Fatal("progress disclosed message identity")
			}
		})
	}
}

func TestRebuildMatchingFailurePreservesPreflightSafety(t *testing.T) {
	var output bytes.Buffer
	failure := errors.New("matching unavailable")
	destination := &progressDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}, before: func(string) {}, inventory: func() ([]Message, error) { return nil, failure }}
	state := rebuildState("is missing")
	state.EmailState = "old"
	state.GmailHistoryID = 7
	stateStore := &fakeStore{state: state}
	_, err := (&Engine{Source: oneMessageSource(), Destination: destination, Store: stateStore, Options: Options{Apply: true, Output: &output}}).Run(t.Context())
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v, want %v", err, failure)
	}
	t.Logf("failed preflight: %v", err)
	if strings.Contains(output.String(), "rebuild: matched") {
		t.Fatal("failed search reported as completed")
	}
	if stateStore.saves != 0 || state.EmailState != "old" || state.GmailHistoryID != 7 || state.Incomplete {
		t.Fatalf("failed matching advanced state: %#v", state)
	}
	if len(destination.created)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
		t.Fatal("failed matching mutated Gmail")
	}
}

func TestIncrementalSyncDoesNotPrintRebuildProgress(t *testing.T) {
	state := store.New()
	state.EmailState = "s1"
	source := oneMessageSource()
	source.changes = []Changes{{NewState: "s2", Updated: []string{"e1"}}}
	var output bytes.Buffer
	_, err := (&Engine{Source: source, Destination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}, Store: &fakeStore{state: state}, Options: Options{Output: &output}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuildProgressLines(output.String())) != 0 {
		t.Fatalf("incremental sync printed rebuild progress:\n%s", output.String())
	}
}

func TestRebuildInvalidMetadataDoesNotReportCompletion(t *testing.T) {
	for _, test := range []struct {
		name       string
		emails     []Email
		wantError  string
		wantCounts []string
	}{
		{"missing", nil, "snapshot was incomplete", []string{"0/1", "0/1"}},
		{"malformed", []Email{{ID: "unexpected"}}, "unrequested email", []string{"0/1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			source := &progressSource{fakeSource: oneMessageSource(), before: func(string) {}, fetch: func([]string) ([]Email, error) { return test.emails, nil }}
			stateStore := &fakeStore{state: rebuildState("is missing")}
			destination := &fakeDestination{labels: standardLabels(), messages: map[string]Message{}}
			engine := &Engine{Source: source, Destination: destination, Store: stateStore, Options: Options{Apply: true, Output: &output}}
			_, err := engine.Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error=%v, want %q", err, test.wantError)
			}
			t.Logf("rejected incomplete or malformed metadata: %v", err)
			want := []string{"rebuild: inventorying Fastmail messages"}
			for _, count := range test.wantCounts {
				want = append(want, "rebuild: loading Fastmail metadata ("+count+" messages)")
			}
			if got := rebuildProgressLines(output.String()); !reflect.DeepEqual(got, want) {
				t.Fatalf("progress=%q, want %q", got, want)
			}
			if stateStore.saves != 0 || engine.snapshot != nil || len(destination.created)+len(destination.inserted)+len(destination.modified)+len(destination.trashed) != 0 {
				t.Fatal("invalid metadata mutated state or Gmail")
			}
		})
	}
}
