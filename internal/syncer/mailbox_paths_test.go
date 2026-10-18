package syncer

import (
	"reflect"
	"testing"
)

func TestMailboxPaths(t *testing.T) {
	got, err := MailboxPaths([]Mailbox{{ID: "c", Name: "Quoted\"\nChild", ParentID: "p"}, {ID: "p", Name: "Inbox"}, {ID: "same", Name: "Inbox"}})
	want := map[string]string{"p": "Inbox", "c": "Inbox/Quoted\"\nChild", "same": "Inbox"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v error=%v want=%v", got, err, want)
	}
	for name, boxes := range map[string][]Mailbox{
		"empty ID": {{Name: "A"}}, "empty name": {{ID: "a"}}, "duplicate ID": {{ID: "a", Name: "A"}, {ID: "a", Name: "B"}},
		"missing parent": {{ID: "a", Name: "A", ParentID: "missing"}}, "cycle": {{ID: "a", Name: "A", ParentID: "b"}, {ID: "b", Name: "B", ParentID: "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MailboxPaths(boxes); err == nil {
				t.Fatal("invalid hierarchy accepted")
			} else {
				t.Logf("expected failure: %v", err)
			}
		})
	}
}
