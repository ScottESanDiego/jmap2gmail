package syncer

import (
	"fmt"
	"slices"
	"strings"
)

const (
	LabelInbox     = "INBOX"
	LabelSent      = "SENT"
	LabelDraft     = "DRAFT"
	LabelImportant = "IMPORTANT"
	LabelStarred   = "STARRED"
	LabelUnread    = "UNREAD"
	LabelSpam      = "SPAM"
	LabelTrash     = "TRASH"
)

// MailboxLabelNames maps stable Fastmail mailbox IDs to Gmail label names.
// A blank name means the JMAP mailbox is virtual (for example, All Mail) and
// does not need a Gmail label.
func MailboxLabelNames(mailboxes []Mailbox, trashName, spamName string) (map[string]string, error) {
	return MailboxLabelNamesWithRoles(mailboxes, trashName, spamName, nil)
}

// MailboxLabelNamesWithRoles applies optional Gmail label-name overrides keyed
// by lower-case JMAP mailbox role (for example, "sent" or "drafts").
func MailboxLabelNamesWithRoles(mailboxes []Mailbox, trashName, spamName string, roleLabels map[string]string) (map[string]string, error) {
	trashName = effectiveRoleLabel("trash", trashName, roleLabels)
	spamName = effectiveRoleLabel("junk", spamName, roleLabels)
	if isSystemLabelName(trashName) || isSystemLabelName(spamName) {
		return nil, fmt.Errorf("Fastmail Trash and Spam must map to Gmail user labels")
	}
	if strings.EqualFold(trashName, spamName) {
		return nil, fmt.Errorf("Fastmail Trash and Spam must use different Gmail labels")
	}
	byID := make(map[string]Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		if mailbox.ID == "" {
			return nil, fmt.Errorf("mailbox has an empty ID")
		}
		byID[mailbox.ID] = mailbox
	}

	paths := make(map[string]string, len(mailboxes))
	visiting := make(map[string]bool, len(mailboxes))
	var path func(string) (string, error)
	path = func(id string) (string, error) {
		if value, ok := paths[id]; ok {
			return value, nil
		}
		mailbox, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("mailbox %q is missing", id)
		}
		if visiting[id] {
			return "", fmt.Errorf("mailbox hierarchy contains a cycle at %q", id)
		}
		visiting[id] = true
		defer delete(visiting, id)

		name := strings.TrimSpace(mailbox.Name)
		if name == "" {
			return "", fmt.Errorf("mailbox %q has an empty name", id)
		}
		if mailbox.ParentID != "" {
			parent, err := path(mailbox.ParentID)
			if err != nil {
				return "", fmt.Errorf("mailbox %q parent: %w", id, err)
			}
			name = parent + "/" + name
		}
		paths[id] = name
		return name, nil
	}

	result := make(map[string]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		var name string
		role := strings.ToLower(mailbox.Role)
		if override, ok := roleLabels[role]; ok {
			name = strings.TrimSpace(override)
		} else {
			switch role {
			case "all":
				name = ""
			case "inbox":
				name = LabelInbox
			case "sent":
				name = LabelSent
			case "drafts":
				name = LabelDraft
			case "important":
				name = LabelImportant
			case "flagged":
				name = LabelStarred
			case "trash":
				name = trashName
			case "junk":
				name = spamName
			default:
				var err error
				name, err = path(mailbox.ID)
				if err != nil {
					return nil, err
				}
			}
		}
		if name == "" && role != "all" {
			return nil, fmt.Errorf("mailbox %q maps to an empty Gmail label", mailbox.ID)
		}
		if strings.EqualFold(name, LabelSpam) {
			return nil, fmt.Errorf("mailbox %q cannot map to Gmail's native Spam label", mailbox.ID)
		}
		result[mailbox.ID] = name
	}
	return result, nil
}

func effectiveRoleLabel(role, fallback string, roleLabels map[string]string) string {
	if override, ok := roleLabels[role]; ok {
		return strings.TrimSpace(override)
	}
	return strings.TrimSpace(fallback)
}

func isSystemLabelName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case LabelInbox, LabelSent, LabelDraft, LabelImportant, LabelStarred, LabelUnread, LabelSpam, LabelTrash:
		return true
	default:
		return false
	}
}

// DesiredLabelNames returns a stable, deduplicated set of Gmail label names.
func DesiredLabelNames(email Email, mailboxNames map[string]string) ([]string, error) {
	set := make(map[string]bool)
	for mailboxID, present := range email.MailboxIDs {
		if !present {
			continue
		}
		name, ok := mailboxNames[mailboxID]
		if !ok {
			return nil, fmt.Errorf("email %q references unknown mailbox %q", email.ID, mailboxID)
		}
		if name != "" {
			set[name] = true
		}
	}
	if !email.Keywords["$seen"] {
		set[LabelUnread] = true
	}
	if email.Keywords["$flagged"] {
		set[LabelStarred] = true
	}

	labels := make([]string, 0, len(set))
	for label := range set {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return labels, nil
}

// ManagedLabelNames returns every label the mirror may add or remove. Native
// Gmail Trash is included so a mapped message can be restored while its
// Fastmail source still exists. Native Gmail Spam is never managed.
func ManagedLabelNames(mailboxNames map[string]string) []string {
	set := map[string]bool{
		LabelUnread:  true,
		LabelStarred: true,
		LabelTrash:   true,
	}
	for _, name := range mailboxNames {
		if name != "" && !strings.EqualFold(name, LabelSpam) {
			set[name] = true
		}
	}
	labels := make([]string, 0, len(set))
	for label := range set {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return labels
}
