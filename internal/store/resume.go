package store

import (
	"errors"
	"maps"
	"slices"
	"strings"
)

const ResumeVersion = 1

// SourceEmail contains metadata only. MIME and credentials never enter state.
type SourceEmail struct {
	ID         string          `json:"id"`
	BlobID     string          `json:"blob_id"`
	MailboxIDs map[string]bool `json:"mailbox_ids"`
	Keywords   map[string]bool `json:"keywords"`
	MessageIDs []string        `json:"message_ids,omitempty"`
	Size       uint64          `json:"size,omitempty"`
}

type WorkItem struct {
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
}

// InsertIntent is written before an insert and cleared with its saved mapping.
// Its presence requires identity recovery before the operation may be retried.
type InsertIntent struct {
	SourceID  string `json:"source_id"`
	Account   string `json:"account"`
	MessageID string `json:"message_id"`
}

type Resume struct {
	Version            int           `json:"version"`
	Mode               string        `json:"mode"`
	Phase              string        `json:"phase"`
	SourceCursor       string        `json:"source_cursor"`
	GmailHistoryID     uint64        `json:"gmail_history_id"`
	Work               []WorkItem    `json:"work"`
	Next               int           `json:"next"`
	ExtraIDs           []string      `json:"extra_ids,omitempty"`
	ExtraNext          int           `json:"extra_next"`
	OldManagedLabelIDs []string      `json:"old_managed_label_ids,omitempty"`
	InsertIntent       *InsertIntent `json:"insert_intent,omitempty"`
}

func validInsertIdentity(state *State, intent *InsertIntent) bool {
	return intent != nil && strings.TrimSpace(intent.SourceID) != "" &&
		strings.TrimSpace(state.SourceAccount) != "" && intent.Account == state.SourceAccount &&
		strings.TrimSpace(intent.MessageID) != "" && strings.TrimSpace(state.GmailAccount) != ""
}

func validateResume(state *State) error {
	for id, email := range state.SourceEmails {
		if strings.TrimSpace(id) == "" || email.ID != id || email.MailboxIDs == nil || email.Keywords == nil {
			return errors.New("invalid cached source metadata")
		}
		for mailboxID := range email.MailboxIDs {
			if strings.TrimSpace(mailboxID) == "" {
				return errors.New("invalid cached source mailbox")
			}
		}
	}
	if state.SourceEmailsComplete && (state.SourceEmails == nil || state.PolicyFingerprint == "") {
		return errors.New("incomplete source metadata cache")
	}
	r := state.Resume
	if r == nil {
		return nil
	}
	if !state.Incomplete || r.Version != ResumeVersion ||
		(r.Mode != "full" && r.Mode != "incremental") || strings.TrimSpace(r.SourceCursor) == "" ||
		r.GmailHistoryID == 0 || state.MailboxScope == "" || state.PolicyFingerprint == "" {
		return errors.New("invalid resume identity or baseline")
	}
	if r.Mode == "incremental" && state.EmailState == "" || r.Mode == "full" && !state.SourceEmailsComplete {
		return errors.New("invalid resume source boundary")
	}
	switch r.Phase {
	case "labels", "source", "catchup", "caught-up", "extras", "commit":
	default:
		return errors.New("invalid resume phase")
	}
	if r.Next < 0 || r.Next > len(r.Work) || r.ExtraNext < 0 || r.ExtraNext > len(r.ExtraIDs) {
		return errors.New("invalid resume progress")
	}
	if (r.Phase == "extras" || r.Phase == "commit") && r.Next != len(r.Work) || r.Phase == "commit" && r.ExtraNext != len(r.ExtraIDs) {
		return errors.New("resume phase skips unfinished work")
	}
	seen := make(map[string]bool, len(r.Work))
	for _, item := range r.Work {
		if strings.TrimSpace(item.SourceID) == "" || seen[item.SourceID] || (item.Kind != "reconcile" && item.Kind != "destroy") {
			return errors.New("invalid or conflicting resume work")
		}
		if item.Kind == "reconcile" {
			if _, ok := state.SourceEmails[item.SourceID]; !ok {
				return errors.New("resume work lacks source metadata")
			}
		}
		seen[item.SourceID] = true
	}
	for _, values := range [][]string{r.ExtraIDs, r.OldManagedLabelIDs} {
		seen := make(map[string]bool, len(values))
		for _, id := range values {
			if strings.TrimSpace(id) == "" || seen[id] {
				return errors.New("invalid or duplicate resume destination ID")
			}
			seen[id] = true
		}
	}
	if r.InsertIntent != nil {
		if !validInsertIdentity(state, r.InsertIntent) || r.Next == len(r.Work) ||
			r.Work[r.Next].SourceID != r.InsertIntent.SourceID || r.Work[r.Next].Kind != "reconcile" {
			return errors.New("invalid insert intent")
		}
	}
	return nil
}

// Clone isolates dry-run planning, including every nested journal collection.
func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	clone := *s
	clone.Messages = maps.Clone(s.Messages)
	clone.Mailboxes = maps.Clone(s.Mailboxes)
	clone.SourceEmails = maps.Clone(s.SourceEmails)
	for id, email := range clone.SourceEmails {
		email.MailboxIDs = maps.Clone(email.MailboxIDs)
		email.Keywords = maps.Clone(email.Keywords)
		email.MessageIDs = slices.Clone(email.MessageIDs)
		clone.SourceEmails[id] = email
	}
	if s.Resume != nil {
		r := *s.Resume
		r.Work = slices.Clone(r.Work)
		r.ExtraIDs = slices.Clone(r.ExtraIDs)
		r.OldManagedLabelIDs = slices.Clone(r.OldManagedLabelIDs)
		if r.InsertIntent != nil {
			intent := *r.InsertIntent
			r.InsertIntent = &intent
		}
		clone.Resume = &r
	}
	return &clone
}
