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
	Version        int                `json:"version"`
	SourceAccount  string             `json:"source_account,omitempty"`
	GmailAccount   string             `json:"gmail_account,omitempty"`
	EmailState     string             `json:"email_state,omitempty"`
	MailboxScope   string             `json:"mailbox_scope,omitempty"`
	GmailHistoryID uint64             `json:"gmail_history_id,omitempty"`
	Incomplete     bool               `json:"incomplete,omitempty"`
	Messages       map[string]Message `json:"messages"`
	Mailboxes      map[string]Mailbox `json:"mailboxes"`
	Rebuild        bool               `json:"-"`
	RebuildReason  string             `json:"-"`
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
		return newRebuild("uses an unsupported format"), nil
	}
	if err := validate(&state); err != nil {
		if state.Incomplete {
			rebuild := newRebuild("was not committed")
			rebuild.Incomplete = true
			return rebuild, nil
		}
		return newRebuild("is invalid"), nil
	}
	if state.Incomplete {
		state.Rebuild = true
		state.RebuildReason = "was not committed"
	}
	return &state, nil
}

func (f File) Save(state *State) error {
	if state == nil {
		return errors.New("cannot save nil state")
	}
	state.Version = CurrentVersion
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".jmapsync-state-*")
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
	return nil
}

func validate(state *State) error {
	if state.SourceAccount == "" || state.GmailAccount == "" || state.EmailState == "" {
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
	return nil
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
