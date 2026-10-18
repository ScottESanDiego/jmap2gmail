package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ResolveMailboxIDs resolves case-sensitive full source paths. Exact paths
// select only that mailbox; terminal /* selects every descendant, excluding the
// parent itself. Every selector must match. No paths means the whole account.
func ResolveMailboxIDs(mailboxes []Mailbox, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	resolved, err := MailboxPaths(mailboxes)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string][]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		path := resolved[mailbox.ID]
		byPath[path] = append(byPath[path], mailbox.ID)
	}
	var ids []string
	for _, path := range paths {
		descendants := strings.HasSuffix(path, "/*")
		if strings.Contains(path, "*") && (!descendants || strings.Count(path, "*") != 1 || len(path) == 2) {
			return nil, fmt.Errorf("fastmail.include_mailboxes: unsupported mailbox selector %q; only a terminal /* is supported", path)
		}
		if descendants {
			parent := strings.TrimSuffix(path, "/*")
			if len(byPath[parent]) == 0 {
				return nil, fmt.Errorf("fastmail.include_mailboxes: unknown mailbox path %q", path)
			}
			if len(byPath[parent]) != 1 {
				return nil, fmt.Errorf("fastmail.include_mailboxes: ambiguous mailbox path %q", path)
			}
			prefix := parent + "/"
			matched := false
			for candidate, matches := range byPath {
				if !strings.HasPrefix(candidate, prefix) {
					continue
				}
				if len(matches) != 1 {
					return nil, fmt.Errorf("fastmail.include_mailboxes: ambiguous mailbox path %q matched by %q", candidate, path)
				}
				ids = append(ids, matches[0])
				matched = true
			}
			if !matched {
				return nil, fmt.Errorf("fastmail.include_mailboxes: mailbox selector %q matched no descendants", path)
			}
			continue
		}
		matches := byPath[path]
		if len(matches) == 0 {
			return nil, fmt.Errorf("fastmail.include_mailboxes: unknown mailbox path %q", path)
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("fastmail.include_mailboxes: ambiguous mailbox path %q", path)
		}
		ids = append(ids, matches[0])
	}
	return sortedUniqueStrings(ids), nil
}

// MailboxPaths returns case-sensitive full source paths keyed by mailbox ID.
// It rejects empty IDs or names, duplicate IDs, missing parents, and cycles.
func MailboxPaths(mailboxes []Mailbox) (map[string]string, error) {
	byID := make(map[string]Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		if mailbox.ID == "" || mailbox.Name == "" {
			return nil, errors.New("mailbox has an empty ID or name")
		}
		if _, exists := byID[mailbox.ID]; exists {
			return nil, fmt.Errorf("duplicate mailbox ID %q", mailbox.ID)
		}
		byID[mailbox.ID] = mailbox
	}
	resolved := make(map[string]string, len(mailboxes))
	visiting := make(map[string]bool, len(mailboxes))
	var pathFor func(string) (string, error)
	pathFor = func(id string) (string, error) {
		if path, ok := resolved[id]; ok {
			return path, nil
		}
		mailbox, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("mailbox parent %q is missing", id)
		}
		if visiting[id] {
			return "", fmt.Errorf("mailbox hierarchy contains a cycle at %q", id)
		}
		visiting[id] = true
		defer delete(visiting, id)
		path := mailbox.Name
		if mailbox.ParentID != "" {
			parent, err := pathFor(mailbox.ParentID)
			if err != nil {
				return "", err
			}
			path = parent + "/" + path
		}
		resolved[id] = path
		return path, nil
	}
	for _, mailbox := range mailboxes {
		if _, err := pathFor(mailbox.ID); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func (e *Engine) prepareMailboxScope(mailboxes []Mailbox) error {
	ids, err := ResolveMailboxIDs(mailboxes, e.Options.IncludeMailboxes)
	if err != nil {
		return err
	}
	if len(ids) != 0 {
		if _, ok := e.Source.(SourceMailboxQuery); !ok {
			return errors.New("Fastmail source does not support mailbox-filtered inventory")
		}
		e.selectedMailboxes = sliceSet(ids)
	}
	scope := mailboxScopeFingerprint(ids, e.Options.Delete)
	e.scopeChanged = e.state.MailboxScope != scope
	e.state.MailboxScope = scope
	return nil
}

func (e *Engine) selectedMailboxList(mailboxes []Mailbox) []Mailbox {
	if e.selectedMailboxes == nil {
		return mailboxes
	}
	selected := make([]Mailbox, 0, len(e.selectedMailboxes))
	for _, mailbox := range mailboxes {
		if e.selectedMailboxes[mailbox.ID] {
			selected = append(selected, mailbox)
		}
	}
	return selected
}

func (e *Engine) emailInScope(email Email) bool {
	if e.selectedMailboxes == nil {
		return true
	}
	for id, present := range email.MailboxIDs {
		if present && e.selectedMailboxes[id] {
			return true
		}
	}
	return false
}

func (e *Engine) projectEmail(email Email) Email {
	if e.selectedMailboxes == nil {
		return email
	}
	membership := make(map[string]bool)
	for id, present := range email.MailboxIDs {
		if present && e.selectedMailboxes[id] {
			membership[id] = true
		}
	}
	email.MailboxIDs = membership
	return email
}

// The policy revision and deletion setting also invalidate old cache assumptions.
func mailboxScopeFingerprint(ids []string, deleteEnabled bool) string {
	encoded, _ := json.Marshal(struct {
		Mirror    int      `json:"mirror"`
		Delete    bool     `json:"delete"`
		Mailboxes []string `json:"mailboxes"`
	}{Mirror: 1, Delete: deleteEnabled, Mailboxes: ids})
	return string(encoded)
}
