package syncer

import (
	"reflect"
	"strings"
	"testing"
)

func selectorMailboxes() []Mailbox {
	return []Mailbox{
		{ID: "root", Name: "Inbox"},
		{ID: "example", Name: "Example", ParentID: "root"},
		{ID: "child", Name: "Child", ParentID: "example"},
		{ID: "deep", Name: "Deep", ParentID: "child"},
		{ID: "examples", Name: "Examples", ParentID: "root"},
		{ID: "neighbor", Name: "Child", ParentID: "examples"},
		{ID: "lists", Name: "MailingLists"},
		{ID: "gentoo", Name: "Gentoo", ParentID: "lists"},
		{ID: "mailers", Name: "Mailers", ParentID: "lists"},
	}
}

func TestMailboxSelectorsExactAndDescendants(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paths, want []string
	}{
		{name: "omitted all"},
		{name: "empty all", paths: []string{}},
		{name: "exact excludes children", paths: []string{"Inbox/Example"}, want: []string{"example"}},
		{name: "descendants exclude parent and neighbor", paths: []string{"Inbox/Example/*"}, want: []string{"child", "deep"}},
		{name: "include parent explicitly", paths: []string{"Inbox/Example", "Inbox/Example/*"}, want: []string{"child", "deep", "example"}},
		{name: "mailing lists", paths: []string{"MailingLists/*"}, want: []string{"gentoo", "mailers"}},
		{name: "overlap deduplicated", paths: []string{"Inbox/Example/*", "Inbox/Example/Child", "Inbox/Example/Child/*", "Inbox/Example/*"}, want: []string{"child", "deep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMailboxIDs(selectorMailboxes(), tc.paths)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("IDs=%v error=%v want %v", got, err, tc.want)
			}
		})
	}
}

func TestMailboxSelectorsRejectZeroMatchesAndUnsupportedPatterns(t *testing.T) {
	for _, path := range []string{"inbox/Example/*", "Inbox/Missing/*", "Inbox/Example/Child/Deep/*", "/*", "*", "Inbox/*/Child", "Inbox/Ex*", "Inbox/**", "Inbox/*/*", "Inbox/Example?"} {
		t.Run(path, func(t *testing.T) {
			_, err := ResolveMailboxIDs(selectorMailboxes(), []string{path})
			if err == nil || !strings.Contains(err.Error(), "fastmail.include_mailboxes") || !strings.Contains(err.Error(), path) {
				t.Fatalf("selector %q error=%v", path, err)
			}
		})
	}
	// One valid selector cannot hide another selector's empty match.
	if _, err := ResolveMailboxIDs(selectorMailboxes(), []string{"MailingLists/*", "Inbox/Example/Child/Deep/*"}); err == nil {
		t.Fatal("empty selector accepted alongside a match")
	}
}

func TestMailboxSelectorsCanonicalIDsIgnoreOrdering(t *testing.T) {
	boxes := selectorMailboxes()
	first, err := ResolveMailboxIDs(boxes, []string{"MailingLists/*", "Inbox/Example/*"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveMailboxIDs(boxes, []string{"Inbox/Example/Child/Deep", "Inbox/Example/*", "MailingLists/Mailers", "MailingLists/Gentoo"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("equivalent selectors produce different IDs: %v vs %v", first, second)
	}
}
