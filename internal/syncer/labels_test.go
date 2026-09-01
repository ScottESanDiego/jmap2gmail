package syncer

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMailboxAndKeywordMapping(t *testing.T) {
	mailboxes := []Mailbox{
		{ID: "inbox", Name: "Inbox", Role: "inbox"},
		{ID: "trash", Name: "Trash", Role: "trash"},
		{ID: "spam", Name: "Spam", Role: "junk"},
		{ID: "projects", Name: "Projects"},
		{ID: "alpha", Name: "Alpha", ParentID: "projects"},
		{ID: "all", Name: "All Mail", Role: "all"},
	}
	names, err := MailboxLabelNames(mailboxes, "Fastmail_Trash", "Fastmail_Spam")
	if err != nil {
		t.Fatal(err)
	}
	wantNames := map[string]string{
		"inbox": LabelInbox, "trash": "Fastmail_Trash", "spam": "Fastmail_Spam",
		"projects": "Projects", "alpha": "Projects/Alpha", "all": "",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("mailbox names = %#v, want %#v", names, wantNames)
	}

	email := Email{
		ID:         "e1",
		MailboxIDs: map[string]bool{"inbox": true, "alpha": true, "all": true},
		Keywords:   map[string]bool{"$flagged": true},
	}
	got, err := DesiredLabelNames(email, names)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{LabelInbox, "Projects/Alpha", LabelStarred, LabelUnread}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desired labels = %#v, want %#v", got, want)
	}
}

func TestMailboxHierarchyCycleFails(t *testing.T) {
	_, err := MailboxLabelNames([]Mailbox{
		{ID: "a", Name: "A", ParentID: "b"},
		{ID: "b", Name: "B", ParentID: "a"},
	}, "Fastmail_Trash", "Fastmail_Spam")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want hierarchy cycle", err)
	}
}

func TestUnknownMailboxFails(t *testing.T) {
	_, err := DesiredLabelNames(Email{
		ID: "e1", MailboxIDs: map[string]bool{"missing": true},
	}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "unknown mailbox") {
		t.Fatalf("error = %v, want unknown mailbox", err)
	}
}

func TestFastmailTrashAndSpamRequireDistinctUserLabels(t *testing.T) {
	mailboxes := []Mailbox{
		{ID: "trash", Name: "Trash", Role: "trash"},
		{ID: "spam", Name: "Spam", Role: "junk"},
	}
	for _, test := range []struct {
		name, trash, spam string
	}{
		{name: "native trash", trash: LabelTrash, spam: "Fastmail_Spam"},
		{name: "native spam", trash: "Fastmail_Trash", spam: LabelSpam},
		{name: "same label", trash: "Fastmail", spam: "fastmail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := MailboxLabelNames(mailboxes, test.trash, test.spam); err == nil {
				t.Fatal("expected invalid special-mailbox labels to fail")
			}
		})
	}
}

func TestMailboxMappingRejectsNativeSpamFromAnyMailbox(t *testing.T) {
	tests := []struct {
		name       string
		mailboxes  []Mailbox
		roleLabels map[string]string
	}{
		{
			name:      "ordinary mailbox",
			mailboxes: []Mailbox{{ID: "custom", Name: "SPAM"}},
		},
		{
			name:       "role override",
			mailboxes:  []Mailbox{{ID: "sent", Name: "Sent", Role: "sent"}},
			roleLabels: map[string]string{"sent": "spam"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := MailboxLabelNamesWithRoles(test.mailboxes, "Fastmail_Trash", "Fastmail_Spam", test.roleLabels)
			if err == nil || !strings.Contains(err.Error(), "native Spam") {
				t.Fatalf("error = %v, want native Spam rejection", err)
			}
		})
	}
}

func TestManagedLabelsExcludeNativeSpam(t *testing.T) {
	labels := ManagedLabelNames(map[string]string{
		"trash": "Fastmail_Trash",
		"spam":  "Fastmail_Spam",
		"bad":   LabelSpam,
	})
	if slices.Contains(labels, LabelSpam) {
		t.Fatalf("managed labels include native Gmail spam: %v", labels)
	}
	if !slices.Contains(labels, LabelTrash) {
		t.Fatalf("managed labels omit native Gmail trash: %v", labels)
	}
}
