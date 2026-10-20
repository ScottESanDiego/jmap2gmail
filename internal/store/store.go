package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const CurrentVersion = 1

type Message struct {
	GmailID       string `json:"gmail_id"`
	SourceDeleted bool   `json:"source_deleted,omitempty"`
}

type Mailbox struct {
	GmailLabelID string `json:"gmail_label_id"`
	Name         string `json:"name"`
}

type State struct {
	Version              int                    `json:"version"`
	SourceAccount        string                 `json:"source_account,omitempty"`
	GmailAccount         string                 `json:"gmail_account,omitempty"`
	EmailState           string                 `json:"email_state,omitempty"`
	MailboxScope         string                 `json:"mailbox_scope,omitempty"`
	GmailHistoryID       uint64                 `json:"gmail_history_id,omitempty"`
	Incomplete           bool                   `json:"incomplete,omitempty"`
	Messages             map[string]Message     `json:"messages"`
	Mailboxes            map[string]Mailbox     `json:"mailboxes"`
	SourceEmails         map[string]SourceEmail `json:"source_emails"`
	SourceEmailsComplete bool                   `json:"source_emails_complete,omitempty"`
	PolicyFingerprint    string                 `json:"policy_fingerprint,omitempty"`
	Resume               *Resume                `json:"resume,omitempty"`
	Rebuild              bool                   `json:"-"`
	RebuildReason        string                 `json:"-"`
}

func New() *State {
	return &State{
		Version:   CurrentVersion,
		Messages:  make(map[string]Message),
		Mailboxes: make(map[string]Mailbox),
	}
}

func newRebuild(reason string) *State {
	state := New()
	state.Rebuild = true
	state.RebuildReason = reason
	return state
}

type File struct {
	Path string
}

func (f File) Load() (*State, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return newRebuild("is missing"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return newRebuild("is corrupt"), nil
	}
	if state.Version != CurrentVersion {
		if state.Resume != nil && state.Resume.InsertIntent != nil {
			return nil, errors.New("unsupported state with an unresolved insert; restore a compatible state file before retrying")
		}
		return newRebuild("uses an unsupported format"), nil
	}
	if err := validate(&state); err != nil {
		// An uncertain insert must remain recoverable even when the surrounding
		// journal is invalid. Never discard its identity and blindly insert again.
		if state.Resume != nil && validInsertIdentity(&state, state.Resume.InsertIntent) {
			rebuild := newRebuild("has an invalid resume checkpoint")
			rebuild.SourceAccount = state.SourceAccount
			rebuild.GmailAccount = state.GmailAccount
			rebuild.Incomplete = true
			intent := *state.Resume.InsertIntent
			rebuild.Resume = &Resume{Version: ResumeVersion, InsertIntent: &intent}
			return rebuild, nil
		}
		if state.Resume != nil && state.Resume.InsertIntent != nil {
			return nil, errors.New("invalid unresolved insert identity; repair the state file before retrying")
		}
		if state.Incomplete {
			rebuild := newRebuild("was not committed")
			rebuild.Incomplete = true
			return rebuild, nil
		}
		return newRebuild("is invalid"), nil
	}
	if state.Incomplete && state.Resume == nil {
		state.Rebuild = true
		state.RebuildReason = "was not committed"
	}
	return &state, nil
}

func (f File) Save(state *State) error {
	return f.save(state, syncStateDirectory)
}

func (f File) save(state *State, syncDirectory func(string) error) error {
	if state == nil {
		return errors.New("cannot save nil state")
	}
	state.Version = CurrentVersion
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".jmap2gmail-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect temporary state: %w", err)
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, f.Path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	committed = true
	// The intent must survive a crash before a remote mutation can begin.
	// Renaming alone does not durably commit the directory entry.
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func syncStateDirectory(path string) error {
	// The state lock may have created the directory just before Save. Sync
	// ancestors too, so a newly created directory entry is durable in its parent.
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for {
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return fmt.Errorf("directory %q: %w", path, err)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func validate(state *State) error {
	if state.SourceAccount == "" || state.GmailAccount == "" || (state.EmailState == "" && state.Resume == nil) {
		return errors.New("missing state identity or cursor")
	}
	if state.Messages == nil || state.Mailboxes == nil {
		return errors.New("missing state mappings")
	}
	seen := make(map[string]bool, len(state.Messages))
	for sourceID, message := range state.Messages {
		if sourceID == "" || message.GmailID == "" || seen[message.GmailID] {
			return errors.New("invalid message mapping")
		}
		seen[message.GmailID] = true
	}
	seenLabels := make(map[string]bool, len(state.Mailboxes))
	for mailboxID, mailbox := range state.Mailboxes {
		if mailboxID == "" || mailbox.GmailLabelID == "" || mailbox.Name == "" || seenLabels[mailbox.GmailLabelID] {
			return errors.New("invalid mailbox mapping")
		}
		seenLabels[mailbox.GmailLabelID] = true
	}
	return validateResume(state)
}

// Memory stores state only for the current process. A new Memory always asks
// the engine to rebuild its mapping from the remote services.
type Memory struct {
	state *State
}

func NewMemory() *Memory {
	return &Memory{state: newRebuild("is disabled")}
}

func (m *Memory) Load() (*State, error) {
	return m.state, nil
}

func (m *Memory) Save(state *State) error {
	if state == nil {
		return errors.New("cannot save nil state")
	}
	state.Version = CurrentVersion
	m.state = state
	return nil
}
