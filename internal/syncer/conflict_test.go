package syncer

import (
	"context"
	"errors"
	"fmt"
	"jmap2gmail/internal/store"
	"reflect"
	"strings"
	"testing"
	"time"
)

type conflictSource struct {
	*fakeSource
	calls     int
	summaries map[string]ConflictSummary
	lookup    func(context.Context) error
	ids       []string
}

func (s *conflictSource) ConflictEmails(ctx context.Context, ids []string) (map[string]ConflictSummary, error) {
	s.calls++
	s.ids = append([]string(nil), ids...)
	if s.lookup != nil {
		if err := s.lookup(ctx); err != nil {
			return nil, err
		}
	}
	return s.summaries, nil
}

type conflictDestination struct {
	*fakeDestination
	calls     int
	summaries map[string]ConflictSummary
	lookup    func(context.Context, string) error
}

func (d *conflictDestination) ConflictMessage(ctx context.Context, id string) (ConflictSummary, error) {
	d.calls++
	if d.lookup != nil {
		if err := d.lookup(ctx, id); err != nil {
			return ConflictSummary{}, err
		}
	}
	return d.summaries[id], nil
}

func TestConflictHumanMetadataAndPreflightNoWrites(t *testing.T) {
	for _, twoSource := range []bool{false, true} {
		t.Run(fmt.Sprint(twoSource), func(t *testing.T) {
			base := oneMessageSource()
			ids := []string{"e1"}
			candidates := []Message{{ID: "g1", LabelIDs: []string{"human"}}, {ID: "g2"}}
			if twoSource {
				ids = append(ids, "e2")
				base.ids = append(base.ids, "e2")
				base.emails["e2"] = Email{ID: "e2", MessageIDs: []string{"m1@example.com"}, MailboxIDs: map[string]bool{"inbox": true}}
				candidates = candidates[:1]
			}
			s := &conflictSource{fakeSource: base, summaries: map[string]ConflictSummary{}}
			for _, id := range ids {
				s.summaries[id] = ConflictSummary{Subject: "Source subject", From: "Alice <a@example.com>", To: "Bob", Cc: "Carol", SentAt: "2026-01-01T12:00:00-08:00", ReceivedAt: "2026-01-01T20:00:00Z", LocationIDs: []string{"child"}}
			}
			s.mailboxes = append(s.mailboxes, Mailbox{ID: "parent", Name: "Projects"}, Mailbox{ID: "child", Name: "Mail", ParentID: "parent"})
			d := &conflictDestination{fakeDestination: &fakeDestination{labels: append(standardLabels(), Label{ID: "human", Name: "My mail"}), messages: map[string]Message{}, byMessageID: map[string][]Message{"m1@example.com": candidates}}, summaries: map[string]ConflictSummary{"g1": {Subject: "Gmail subject", From: "Other sender", LocationIDs: []string{"human"}}}}
			st := &fakeStore{state: rebuildState("missing")}
			before := st.state.Clone()
			_, err := (&Engine{Source: s, Destination: d, Store: st, Options: Options{Apply: true}}).Run(t.Context())
			if err == nil {
				t.Fatal("missing ambiguity")
			}
			out := err.Error()
			for _, want := range []string{"adoption is ambiguous", "Source subject", "Gmail subject", "Projects/Mail", "My mail", "cc=\"Carol\"", "2026-01-01T12:00:00-08:00", "in:anywhere -in:spam rfc822msgid:m1@example.com", "in:* msgid:m1@example.com"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
			if s.calls != 1 || d.calls != len(candidates) || st.saves != 0 || len(d.inserted)+len(d.modified)+len(d.created)+len(d.renamed)+len(d.trashed) != 0 {
				t.Fatal("unexpected calls or mutation")
			}
			// Account binding is in-memory during preflight; persisted state is untouched.
			before.SourceAccount = st.state.SourceAccount
			before.GmailAccount = st.state.GmailAccount
			if !reflect.DeepEqual(before, st.state) {
				t.Fatalf("preflight changed cache contents")
			}
		})
	}
}

func TestConflictMetadataLazyOnSuccess(t *testing.T) {
	s := &conflictSource{fakeSource: oneMessageSource()}
	d := &conflictDestination{fakeDestination: &fakeDestination{labels: standardLabels(), messages: map[string]Message{}, byMessageID: map[string][]Message{}}}
	_, err := (&Engine{Source: s, Destination: d, Store: &fakeStore{state: store.New()}}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.calls+d.calls != 0 {
		t.Fatal("normal success fetched human metadata")
	}
}

func TestConflictTimeoutCancellationAndFailuresPreserveCause(t *testing.T) {
	cause := errors.New("original ambiguity")
	for _, which := range []string{"source", "gmail", "failure"} {
		t.Run(which, func(t *testing.T) {
			s := &conflictSource{fakeSource: oneMessageSource()}
			d := &conflictDestination{fakeDestination: &fakeDestination{}}
			canceled := false
			block := func(ctx context.Context) error { <-ctx.Done(); canceled = true; return ctx.Err() }
			switch which {
			case "source":
				s.lookup = block
			case "gmail":
				d.lookup = func(ctx context.Context, _ string) error { return block(ctx) }
			case "failure":
				d.lookup = func(context.Context, string) error { return errors.New("private transport failure") }
			}
			start := time.Now()
			err := (&Engine{Source: s, Destination: d}).conflictErrorWithin(t.Context(), cause, "id@example.com", []string{"e1"}, []Message{{ID: "g1"}, {ID: "g2"}}, 10*time.Millisecond)
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "private transport failure") {
				t.Fatal(err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("timeout exceeded")
			}
			if which != "failure" && !canceled {
				t.Fatal("request not canceled")
			}
		})
	}
	if conflictTimeout != 10*time.Second {
		t.Fatal("production aggregate timeout changed")
	}
}

func TestConflictSafeHeadersMissingAndCap(t *testing.T) {
	s := &conflictSource{fakeSource: oneMessageSource(), summaries: map[string]ConflictSummary{"00": {Subject: "=?UTF-8?Q?Hello_=1B=5B31m?=\r\nforged", From: "\"quoted\"\tname"}}}
	d := &conflictDestination{fakeDestination: &fakeDestination{}}
	var ids []string
	var candidates []Message
	for i := 24; i >= 0; i-- {
		id := fmt.Sprintf("%02d", i)
		ids = append(ids, id)
		candidates = append(candidates, Message{ID: id})
	}
	out := (&Engine{Source: s, Destination: d}).conflictError(t.Context(), errors.New("ambiguous"), "id\n-in:inbox", ids, candidates).Error()
	for _, want := range []string{"Hello \\x1b[31m\\r\\nforged", "queries: unavailable", "Fastmail candidates: 25 (showing 20)", "Gmail candidates: 25 (showing 20)", "truncated", "sent=unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || d.calls != 20 || len(s.ids) != 20 || s.ids[0] != "00" || s.ids[19] != "19" {
		t.Fatal("unsafe or unbounded diagnostics")
	}
	unsupported := (&Engine{Source: oneMessageSource(), Destination: &fakeDestination{}}).conflictError(t.Context(), errors.New("ambiguous"), "id", []string{"e1"}, []Message{{ID: "g1"}}).Error()
	if !strings.Contains(unsupported, "subject=unavailable") {
		t.Fatal(unsupported)
	}
}

func TestIncrementalAmbiguityStopsBeforeMutationAndKeepsEarlierWork(t *testing.T) {
	s := &conflictSource{fakeSource: oneMessageSource()}
	d := &conflictDestination{fakeDestination: &fakeDestination{messages: map[string]Message{}, byMessageID: map[string][]Message{}}}
	state := store.New()
	e := &Engine{Source: s, Destination: d, state: state, Options: Options{Apply: true}, mailboxNames: map[string]string{"inbox": LabelInbox}, labelIDs: map[string]string{LabelInbox: LabelInbox, LabelUnread: LabelUnread, "managed": "managed"}, labelNames: map[string]string{"human": "Human Gmail label"}, managedLabel: "managed", managedIDs: map[string]bool{}, liveSourceIDs: map[string]bool{}}
	e.defaults()
	e.sourceMailboxes = s.mailboxes
	// An earlier completed mapping must survive a later message's failure.
	state.Messages["completed"] = store.Message{GmailID: "earlier"}
	state.SourceEmails = map[string]store.SourceEmail{}
	state.SourceEmails["e1"] = store.SourceEmail{MailboxIDs: map[string]bool{"inbox": true}}
	d.byMessageID["m1@example.com"] = []Message{{ID: "a", LabelIDs: []string{"human"}}, {ID: "b"}}
	err := e.syncEmail(t.Context(), s.emails["e1"])
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") || !strings.Contains(err.Error(), "Human Gmail label") || !strings.Contains(err.Error(), "Inbox") {
		t.Fatal(err)
	}
	if len(d.inserted)+len(d.modified)+len(d.trashed) != 0 || state.Messages["completed"].GmailID != "earlier" || state.Messages["e1"].GmailID != "" {
		t.Fatal("ambiguous incremental message mutated data or earlier work")
	}
}

func TestConflictSearchCommonGmailIDAndOversizedHumanFields(t *testing.T) {
	id := "CAfoo+bar=baz/part@mail.gmail.com"
	if !safeSearchID(id) {
		t.Fatal("common Message-ID rejected")
	}
	oversized := strings.Repeat("界", 2000)
	s := &conflictSource{fakeSource: oneMessageSource(), summaries: map[string]ConflictSummary{"e1": {Subject: oversized, From: oversized}}}
	out := (&Engine{Source: s, Destination: &fakeDestination{}}).conflictError(t.Context(), errors.New("ambiguous"), id, []string{"e1"}, nil).Error()
	if !strings.Contains(out, "rfc822msgid:"+id) || !strings.Contains(out, "msgid:"+id) || strings.Contains(out, oversized) || strings.Count(out, "[truncated]") != 2 {
		t.Fatal("search or field bound incorrect")
	}
	for _, malicious := range []string{"id -in:inbox", "id\" OR from:attacker", "id\r\nfoo", "id\\escape"} {
		if safeSearchID(malicious) {
			t.Fatalf("unsafe ID accepted: %q", malicious)
		}
	}
}

func TestIncrementalAmbiguityDurablyCheckpointsEarlierWork(t *testing.T) {
	source, destination := twoResumeMessages()
	file := store.File{Path: t.TempDir() + "/state.json"}
	seedIncrementalResume(t, file, source, destination)
	delete(destination.messages, "g2")
	destination.byMessageID = map[string][]Message{"m2@example.com": {{ID: "candidate-a"}, {ID: "candidate-b"}}}
	_, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "adoption is ambiguous") {
		t.Fatal(err)
	}
	paused, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if paused.Rebuild || paused.Resume == nil || paused.Resume.Next != 1 || paused.Messages["e1"].GmailID != "g1" || paused.EmailState == source.state {
		t.Fatalf("earlier work not durably checkpointed: %#v", paused)
	}
	if len(destination.inserted)+len(destination.trashed) != 0 || len(destination.modified) != 1 || destination.modified[0].id != "g1" {
		t.Fatalf("failed message mutated Gmail: modified=%v", destination.modified)
	}
	// A fresh engine resumes the failed item after the operator resolves ambiguity.
	destination.byMessageID["m2@example.com"] = nil
	destination.modified = nil
	if _, err := (&Engine{Source: source, Destination: destination, Store: file, Options: Options{Apply: true, Delete: true}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(destination.modified) != 0 || len(destination.inserted) != 1 {
		t.Fatalf("fresh engine repeated completed work: modified=%v inserts=%v", destination.modified, destination.inserted)
	}
}

func TestConflictSearchLiteralRFCAtomPunctuation(t *testing.T) {
	id := "a!#$%&'*?^`{|}~@example.com"
	if !safeSearchID(id) || conflictSearchTerm(id) != "\""+id+"\"" {
		t.Fatal("valid literal RFC atom unsupported")
	}
	for _, messageID := range []string{id, "CAfoo+bar=baz/part@mail.gmail.com"} {
		out := (&Engine{Source: oneMessageSource(), Destination: &fakeDestination{}}).conflictError(t.Context(), errors.New("ambiguous"), messageID, nil, nil).Error()
		want := "Search Gmail:\n  in:anywhere -in:spam rfc822msgid:" + conflictSearchTerm(messageID) + "\nSearch Fastmail:\n  in:* msgid:" + conflictSearchTerm(messageID) + "\n"
		if !strings.Contains(out, want) || strings.Contains(out, `\"`) {
			t.Fatalf("query is not directly copyable: %s", out)
		}
	}

}

func TestConflictMalformedMessageIDQueriesUnavailable(t *testing.T) {
	for _, id := range []string{"garbage", "foo@@example.com", "foo..bar@domain", "@example.com", "foo@", "foo@.example", "foo@example..com", ".foo@example.com", "foo.@example.com"} {
		if safeSearchID(id) {
			t.Errorf("malformed ID accepted: %q", id)
		}
		out := (&Engine{Source: oneMessageSource(), Destination: &fakeDestination{}}).conflictError(t.Context(), errors.New("ambiguous"), id, nil, nil).Error()
		if !strings.Contains(out, "Search queries: unavailable") || strings.Contains(out, "Search Gmail:") {
			t.Errorf("malformed ID generated query: %s", out)
		}
	}
}
