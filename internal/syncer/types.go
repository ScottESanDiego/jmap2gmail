package syncer

import (
	"context"
	"errors"
	"io"
)

var (
	ErrMutationRejected                  = errors.New("destination rejected mutation without applying it")
	ErrNotFound                          = errors.New("destination message not found")
	ErrCannotCalculateChanges            = errors.New("source cannot calculate changes")
	ErrCannotCalculateDestinationChanges = errors.New("destination cannot calculate changes")
)

const (
	HeaderAccount = "X-Jmap2gmail-Account"
	HeaderEmailID = "X-Jmap2gmail-Email-ID"
)

// Mailbox is the subset of a JMAP Mailbox needed to mirror membership.
type Mailbox struct {
	ID       string
	Name     string
	ParentID string
	Role     string
}

// Email is the subset of a JMAP Email needed by the mirror engine.
type Email struct {
	ID         string
	BlobID     string
	MailboxIDs map[string]bool
	Keywords   map[string]bool
	MessageIDs []string
	Size       uint64
}

// Changes is one page of JMAP Email/changes results.
type Changes struct {
	NewState  string
	More      bool
	Created   []string
	Updated   []string
	Destroyed []string
}

// Source exposes Fastmail operations independently of a particular JMAP
// library so mirror policy can be tested without network calls.
type Source interface {
	Mailboxes(context.Context) ([]Mailbox, error)
	State(context.Context) (string, error)
	AllEmailIDs(context.Context) ([]string, error)
	Emails(context.Context, []string) ([]Email, error)
	Changes(context.Context, string) (Changes, error)
	Raw(context.Context, string) (io.ReadCloser, error)
}

// SourceMailboxQuery provides a server-side inventory restricted to mailboxes.
// Email metadata and change events remain account-wide.
type SourceMailboxQuery interface {
	EmailIDsInMailboxes(context.Context, []string) ([]string, error)
}

type SourceIdentity interface {
	SourceIdentity(context.Context) (string, error)
}

// Label is a Gmail system or user label.
type Label struct {
	ID   string
	Name string
	Type string
}

// Message is the destination metadata needed for reconciliation.
type Message struct {
	ID               string
	LabelIDs         []string
	AccountHeaders   []string
	EmailIDHeaders   []string
	MessageIDHeaders []string
}

// Destination exposes the Gmail operations used by the mirror engine.
type Destination interface {
	Labels(context.Context) ([]Label, error)
	CreateLabel(context.Context, string) (Label, error)
	RenameLabel(context.Context, string, string) (Label, error)
	FindByMessageID(context.Context, string) ([]Message, error)
	Inventory(context.Context) ([]Message, error)
	Message(context.Context, string) (Message, error)
	Insert(context.Context, io.Reader, []string) (Message, error)
	ModifyLabels(context.Context, string, []string, []string) (Message, error)
	Trash(context.Context, string) error
}

type DestinationIdentity interface {
	DestinationIdentity(context.Context) (string, error)
}

// DestinationHistory is implemented by destinations that can efficiently
// report label changes and message deletions made outside jmap2gmail.
type DestinationHistory interface {
	CurrentHistoryID(context.Context) (uint64, error)
	ChangesSince(context.Context, uint64) ([]string, error)
}
