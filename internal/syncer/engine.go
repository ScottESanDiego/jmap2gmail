package syncer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"jmapsync/internal/store"
)

type StateStore interface {
	Load() (*store.State, error)
	Save(*store.State) error
}

type Options struct {
	Apply      bool
	Delete     bool
	Full       bool
	TrashLabel string
	SpamLabel  string
	RoleLabels map[string]string
	BatchSize  int
	Output     io.Writer
}

type Result struct {
	LabelsCreated int
	LabelsRenamed int
	Inserted      int
	Adopted       int
	Modified      int
	Trashed       int
	SkippedTrash  int
	IgnoredSpam   int
	Unchanged     int
}

type Engine struct {
	Source      Source
	Destination Destination
	Store       StateStore
	Options     Options

	state        *store.State
	mailboxNames map[string]string
	labelIDs     map[string]string
	labelNames   map[string]string
	managedIDs   map[string]bool
	managedLabel string
	accountHash  string
	labels       []Label
	snapshot     map[string]Email
	snapshotIDs  []string
	snapshotAt   string
	adoptions    map[string]bool
	adoptionScan map[string][]Message
	result       Result
}

func (e *Engine) Run(ctx context.Context) (Result, error) {
	if e.Source == nil || e.Destination == nil || e.Store == nil {
		return Result{}, errors.New("sync engine requires source, destination, and state store")
	}
	e.defaults()
	state, err := e.Store.Load()
	if err != nil {
		return Result{}, err
	}
	e.state = state
	hadSourceCursor := state.EmailState != ""
	if err := e.bindAccounts(ctx); err != nil {
		return Result{}, err
	}

	mailboxes, err := e.Source.Mailboxes(ctx)
	if err != nil {
		return Result{}, err
	}
	e.mailboxNames, err = MailboxLabelNamesWithRoles(mailboxes, e.Options.TrashLabel, e.Options.SpamLabel, e.Options.RoleLabels)
	if err != nil {
		return Result{}, err
	}
	if e.state.Rebuild {
		e.log("state cache %s; rebuilding from Fastmail and Gmail", e.state.RebuildReason)
		if err := e.preflightRebuild(ctx); err != nil {
			return Result{}, err
		}
	}
	if e.Options.Apply {
		e.state.Incomplete = true
		if err := e.Store.Save(e.state); err != nil {
			return Result{}, err
		}
	}
	if err := e.resolveLabels(ctx, mailboxes); err != nil {
		return Result{}, err
	}
	if err := e.processTombstones(ctx); err != nil {
		return Result{}, err
	}
	historyDestination, hasHistory := e.Destination.(DestinationHistory)
	var historyBaseline uint64
	if hasHistory {
		historyBaseline, err = historyDestination.CurrentHistoryID(ctx)
		if err != nil {
			return Result{}, err
		}
	}

	if e.Options.Full || e.state.EmailState == "" {
		if err := e.full(ctx); err != nil {
			return Result{}, err
		}
	} else if err := e.incremental(ctx); err != nil {
		if !errors.Is(err, ErrCannotCalculateChanges) {
			return Result{}, err
		}
		e.log("Fastmail change history expired; planning a full reconciliation")
		if err := e.full(ctx); err != nil {
			return Result{}, err
		}
	}
	if hasHistory {
		if err := e.reconcileDestination(ctx, historyDestination, hadSourceCursor); err != nil {
			return Result{}, err
		}
		if e.Options.Apply {
			e.state.GmailHistoryID = historyBaseline
			if err := e.Store.Save(e.state); err != nil {
				return Result{}, err
			}
		}
	}

	if e.Options.Apply {
		current := make(map[string]bool, len(mailboxes))
		for _, mailbox := range mailboxes {
			current[mailbox.ID] = true
		}
		for id := range e.state.Mailboxes {
			if !current[id] {
				delete(e.state.Mailboxes, id)
			}
		}
		e.state.Incomplete = false
		e.state.Rebuild = false
		e.state.RebuildReason = ""
		if err := e.Store.Save(e.state); err != nil {
			return Result{}, err
		}
	} else {
		e.log("leave Gmail and state unchanged")
	}
	return e.result, nil
}

func (e *Engine) defaults() {
	if e.Options.TrashLabel == "" {
		e.Options.TrashLabel = "Fastmail_Trash"
	}
	if e.Options.SpamLabel == "" {
		e.Options.SpamLabel = "Fastmail_Spam"
	}
	if e.Options.BatchSize <= 0 {
		e.Options.BatchSize = 500
	}
	if e.Options.Output == nil {
		e.Options.Output = io.Discard
	}
}

func (e *Engine) bindAccounts(ctx context.Context) error {
	source, ok := e.Source.(SourceIdentity)
	if !ok {
		return errors.New("Fastmail source does not expose an account identity")
	}
	identity, err := source.SourceIdentity(ctx)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(identity))
	e.accountHash = hex.EncodeToString(digest[:])
	e.managedLabel = "jmapsync-managed/" + e.accountHash
	if e.state.SourceAccount != "" && e.state.SourceAccount != e.accountHash {
		return errors.New("state belongs to another Fastmail account; remove the state file to rebuild it")
	}
	if e.state.SourceAccount == "" && e.Options.Apply {
		e.state.SourceAccount = e.accountHash
	}
	if destination, ok := e.Destination.(DestinationIdentity); ok {
		identity, err := destination.DestinationIdentity(ctx)
		if err != nil {
			return err
		}
		if e.state.GmailAccount != "" && e.state.GmailAccount != identity {
			return fmt.Errorf("state belongs to Gmail account %q, authenticated account is %q", e.state.GmailAccount, identity)
		}
		if e.state.GmailAccount == "" && e.Options.Apply {
			e.state.GmailAccount = identity
		}
	}
	return nil
}

func (e *Engine) preflightRebuild(ctx context.Context) error {
	if err := e.loadSnapshot(ctx); err != nil {
		return err
	}
	labels, err := e.Destination.Labels(ctx)
	if err != nil {
		return err
	}
	e.labels = labels
	var markerID string
	for _, label := range labels {
		if label.Name == e.managedLabel {
			markerID = label.ID
			break
		}
	}
	if markerID == "" {
		return e.preflightAdoptions(ctx, make(map[string]string))
	}
	messages, err := e.Destination.ManagedMessages(ctx, markerID)
	if err != nil {
		return err
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	claimedGmail := make(map[string]string)
	legacy := make(map[string][]Message)
	for _, message := range messages {
		if hasLabelID(message, LabelSpam) {
			e.result.IgnoredSpam++
			e.log("ignore marked Gmail spam message %q", message.ID)
			continue
		}
		hasAccount := message.AccountHeaders != nil
		hasEmailID := message.EmailIDHeaders != nil
		if !hasAccount && !hasEmailID {
			if len(message.MessageIDHeaders) != 1 {
				return fmt.Errorf("legacy Gmail message %q cannot be identified; restore the cache or remove its marker label", message.ID)
			}
			messageID := strings.Trim(strings.TrimSpace(message.MessageIDHeaders[0]), "<>")
			if messageID == "" {
				return fmt.Errorf("legacy Gmail message %q cannot be identified; restore the cache or remove its marker label", message.ID)
			}
			legacy[messageID] = append(legacy[messageID], message)
			continue
		}
		if len(message.AccountHeaders) != 1 || len(message.EmailIDHeaders) != 1 ||
			strings.TrimSpace(message.AccountHeaders[0]) != e.accountHash ||
			strings.TrimSpace(message.EmailIDHeaders[0]) == "" {
			return fmt.Errorf("managed Gmail message %q has invalid identity headers; remove its marker label or repair the headers", message.ID)
		}
		sourceID := strings.TrimSpace(message.EmailIDHeaders[0])
		if prior := e.state.Messages[sourceID].GmailID; prior != "" && prior != message.ID {
			return fmt.Errorf("Fastmail email %q has duplicate Gmail claims; remove the extra marker label", sourceID)
		}
		if priorSource := claimedGmail[message.ID]; priorSource != "" && priorSource != sourceID {
			return fmt.Errorf("Gmail identity claims conflict; remove the incorrect marker label")
		}
		e.state.Messages[sourceID] = store.Message{GmailID: message.ID}
		claimedGmail[message.ID] = sourceID
	}

	sourcesByMessageID := make(map[string][]string)
	for sourceID, email := range e.snapshot {
		if e.state.Messages[sourceID].GmailID != "" {
			continue
		}
		messageID := effectiveMessageID(email)
		sourcesByMessageID[messageID] = append(sourcesByMessageID[messageID], sourceID)
	}
	legacyMessageIDs := make([]string, 0, len(legacy))
	for messageID := range legacy {
		legacyMessageIDs = append(legacyMessageIDs, messageID)
	}
	sort.Strings(legacyMessageIDs)
	for _, messageID := range legacyMessageIDs {
		candidates := legacy[messageID]
		sourceIDs := sourcesByMessageID[messageID]
		if len(candidates) > 1 || len(sourceIDs) > 1 {
			return fmt.Errorf("legacy Gmail mapping is ambiguous; restore the cache or remove extra marker labels")
		}
		if len(candidates) == 1 && len(sourceIDs) == 1 {
			if prior := claimedGmail[candidates[0].ID]; prior != "" && prior != sourceIDs[0] {
				return fmt.Errorf("legacy Gmail mapping conflicts with identity headers; repair the marker labels")
			}
			e.state.Messages[sourceIDs[0]] = store.Message{GmailID: candidates[0].ID}
			claimedGmail[candidates[0].ID] = sourceIDs[0]
		}
	}
	if err := e.preflightAdoptions(ctx, claimedGmail); err != nil {
		return err
	}
	return nil
}

func (e *Engine) preflightAdoptions(ctx context.Context, claimedGmail map[string]string) error {
	e.adoptions = make(map[string]bool)
	e.adoptionScan = make(map[string][]Message)
	byMessageID := make(map[string][]string)
	for sourceID, email := range e.snapshot {
		if e.state.Messages[sourceID].GmailID == "" {
			byMessageID[effectiveMessageID(email)] = append(byMessageID[effectiveMessageID(email)], sourceID)
		}
	}
	messageIDs := make([]string, 0, len(byMessageID))
	for messageID := range byMessageID {
		messageIDs = append(messageIDs, messageID)
	}
	sort.Strings(messageIDs)
	for _, messageID := range messageIDs {
		candidates, err := e.Destination.FindByMessageID(ctx, messageID)
		if err != nil {
			return err
		}
		candidates = e.withoutSpamCandidates(candidates)
		candidates = e.withoutIdentityCandidates(candidates)
		sourceIDs := byMessageID[messageID]
		sort.Strings(sourceIDs)
		if len(candidates) > 1 || (len(candidates) == 1 && len(sourceIDs) > 1) {
			return fmt.Errorf("Gmail Message-ID adoption is ambiguous; remove duplicate messages or add identity headers")
		}
		for _, sourceID := range sourceIDs {
			e.adoptionScan[sourceID] = candidates
		}
		if len(candidates) == 1 {
			if prior := claimedGmail[candidates[0].ID]; prior != "" && prior != sourceIDs[0] {
				return fmt.Errorf("Gmail Message-ID adoption conflicts with identity headers; repair the marker labels")
			}
			e.state.Messages[sourceIDs[0]] = store.Message{GmailID: candidates[0].ID}
			e.adoptions[sourceIDs[0]] = true
			claimedGmail[candidates[0].ID] = sourceIDs[0]
		}
	}
	return nil
}

func (e *Engine) loadSnapshot(ctx context.Context) error {
	baseline, err := e.Source.State(ctx)
	if err != nil {
		return err
	}
	ids, err := e.Source.AllEmailIDs(ctx)
	if err != nil {
		return err
	}
	sort.Strings(ids)
	snapshot := make(map[string]Email, len(ids))
	for start := 0; start < len(ids); start += e.Options.BatchSize {
		end := min(start+e.Options.BatchSize, len(ids))
		emails, err := e.Source.Emails(ctx, ids[start:end])
		if err != nil {
			return err
		}
		for _, email := range emails {
			snapshot[email.ID] = email
		}
	}
	if len(snapshot) != len(ids) {
		return errors.New("Fastmail snapshot was incomplete; retry the sync")
	}
	e.snapshotAt = baseline
	e.snapshotIDs = append([]string(nil), ids...)
	e.snapshot = snapshot
	return nil
}

func (e *Engine) resolveLabels(ctx context.Context, mailboxes []Mailbox) error {
	labels := e.labels
	if labels == nil {
		var err error
		labels, err = e.Destination.Labels(ctx)
		if err != nil {
			return err
		}
	}
	byName := make(map[string]Label, len(labels))
	byID := make(map[string]Label, len(labels))
	for _, label := range labels {
		byName[label.Name] = label
		byID[label.ID] = label
	}
	e.labelIDs = make(map[string]string, len(labels))
	e.labelNames = make(map[string]string, len(labels))
	for name, label := range byName {
		e.labelIDs[name] = label.ID
		e.labelNames[label.ID] = name
	}
	e.managedIDs = make(map[string]bool)
	for _, saved := range e.state.Mailboxes {
		if saved.GmailLabelID != "" {
			e.managedIDs[saved.GmailLabelID] = true
		}
	}
	marker, found := byName[e.managedLabel]
	if !found {
		if e.Options.Apply {
			var err error
			marker, err = e.Destination.CreateLabel(ctx, e.managedLabel)
			if err != nil {
				return err
			}
		} else {
			marker = Label{Name: e.managedLabel, Type: "user"}
		}
		e.result.LabelsCreated++
		e.log("create Gmail label %q", e.managedLabel)
	}
	if marker.ID != "" {
		e.labelIDs[e.managedLabel] = marker.ID
		e.labelNames[marker.ID] = e.managedLabel
		e.managedIDs[marker.ID] = true
	}

	var err error
	orderedMailboxes := append([]Mailbox(nil), mailboxes...)
	sort.Slice(orderedMailboxes, func(i, j int) bool {
		return e.mailboxNames[orderedMailboxes[i].ID] < e.mailboxNames[orderedMailboxes[j].ID]
	})
	for _, mailbox := range orderedMailboxes {
		name := e.mailboxNames[mailbox.ID]
		if name == "" {
			continue
		}
		label, found := byName[name]
		if !found {
			if saved, ok := e.state.Mailboxes[mailbox.ID]; ok {
				if old, exists := byID[saved.GmailLabelID]; exists && old.Type == "user" {
					if e.Options.Apply {
						label, err = e.Destination.RenameLabel(ctx, old.ID, name)
						if err != nil {
							return err
						}
					} else {
						label = Label{ID: old.ID, Name: name, Type: "user"}
					}
					e.result.LabelsRenamed++
					e.log("rename Gmail label %q to %q", old.Name, name)
					found = true
				}
			}
		}
		if !found {
			if isSystemLabelName(name) {
				return fmt.Errorf("Gmail system label %q is unavailable", name)
			}
			if e.Options.Apply {
				label, err = e.Destination.CreateLabel(ctx, name)
				if err != nil {
					return err
				}
			} else {
				label = Label{Name: name, Type: "user"}
			}
			e.result.LabelsCreated++
			e.log("create Gmail label %q", name)
		}
		if label.ID != "" {
			e.labelIDs[name] = label.ID
			e.labelNames[label.ID] = name
			e.managedIDs[label.ID] = true
		}
		if e.Options.Apply {
			e.state.Mailboxes[mailbox.ID] = store.Mailbox{GmailLabelID: label.ID, Name: name}
		}
	}
	for _, name := range ManagedLabelNames(e.mailboxNames) {
		if id := e.labelIDs[name]; id != "" {
			e.managedIDs[id] = true
		}
	}
	if e.Options.Apply {
		return e.Store.Save(e.state)
	}
	return nil
}

func (e *Engine) full(ctx context.Context) error {
	if e.snapshot == nil {
		if err := e.loadSnapshot(ctx); err != nil {
			return err
		}
	}
	baseline := e.snapshotAt
	ids := e.snapshotIDs
	e.log("Fastmail snapshot contains %d messages", len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for start := 0; start < len(ids); start += e.Options.BatchSize {
		end := min(start+e.Options.BatchSize, len(ids))
		for _, id := range ids[start:end] {
			sourceEmail := e.snapshot[id]
			if err := e.syncEmail(ctx, sourceEmail); err != nil {
				return fmt.Errorf("sync Fastmail email %q: %w", sourceEmail.ID, err)
			}
		}
	}
	for _, sourceID := range sortedMessageKeys(e.state.Messages) {
		if !seen[sourceID] {
			if err := e.destroy(ctx, sourceID); err != nil {
				return err
			}
		}
	}
	if e.Options.Apply {
		e.state.EmailState = baseline
		if err := e.Store.Save(e.state); err != nil {
			return err
		}
	}
	return e.incrementalFrom(ctx, baseline)
}

func (e *Engine) incremental(ctx context.Context) error {
	return e.incrementalFrom(ctx, e.state.EmailState)
}

func (e *Engine) incrementalFrom(ctx context.Context, since string) error {
	for {
		changes, err := e.Source.Changes(ctx, since)
		if err != nil {
			return err
		}
		for _, id := range sortedUniqueStrings(changes.Destroyed) {
			if err := e.destroy(ctx, id); err != nil {
				return err
			}
		}
		changedIDs := sortedUniqueStrings(append(append([]string(nil), changes.Created...), changes.Updated...))
		for start := 0; start < len(changedIDs); start += e.Options.BatchSize {
			end := min(start+e.Options.BatchSize, len(changedIDs))
			emails, err := e.Source.Emails(ctx, changedIDs[start:end])
			if err != nil {
				return err
			}
			sort.Slice(emails, func(i, j int) bool { return emails[i].ID < emails[j].ID })
			for _, sourceEmail := range emails {
				if err := e.syncEmail(ctx, sourceEmail); err != nil {
					return fmt.Errorf("sync Fastmail email %q: %w", sourceEmail.ID, err)
				}
			}
		}
		if e.Options.Apply {
			e.state.EmailState = changes.NewState
			if err := e.Store.Save(e.state); err != nil {
				return err
			}
		}
		since = changes.NewState
		if !changes.More {
			return nil
		}
	}
}

func (e *Engine) syncEmail(ctx context.Context, sourceEmail Email) error {
	desiredNames, err := DesiredLabelNames(sourceEmail, e.mailboxNames)
	if err != nil {
		return err
	}
	desiredNames = append(desiredNames, e.managedLabel)
	sort.Strings(desiredNames)
	desiredIDs, complete := e.namesToIDs(desiredNames)
	record, mapped := e.state.Messages[sourceEmail.ID]
	if mapped && record.GmailID != "" {
		if e.adoptions[sourceEmail.ID] {
			e.result.Adopted++
			e.log("adopt Gmail message %q for Fastmail email %q", record.GmailID, sourceEmail.ID)
			delete(e.adoptions, sourceEmail.ID)
		}
		message, err := e.Destination.Message(ctx, record.GmailID)
		if err == nil {
			if hasLabelID(message, LabelSpam) {
				e.result.IgnoredSpam++
				e.log("ignore Gmail spam message %q", message.ID)
				return e.remember(sourceEmail, record.GmailID, desiredNames)
			}
			if err := e.reconcile(ctx, message, desiredIDs, desiredNames, complete); err != nil {
				return err
			}
			return e.remember(sourceEmail, record.GmailID, desiredNames)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		e.log("Gmail message %q disappeared; reinsert Fastmail email %q", record.GmailID, sourceEmail.ID)
	}

	messageID := effectiveMessageID(sourceEmail)
	if messageID != "" {
		candidates, scanned := e.adoptionScan[sourceEmail.ID]
		if !scanned {
			var err error
			candidates, err = e.Destination.FindByMessageID(ctx, messageID)
			if err != nil {
				return err
			}
			candidates = e.withoutSpamCandidates(candidates)
			candidates = e.withoutIdentityCandidates(candidates)
		}
		if len(candidates) == 1 && !e.gmailIDMappedElsewhere(sourceEmail.ID, candidates[0].ID) {
			e.result.Adopted++
			e.log("adopt Gmail message %q for Fastmail email %q", candidates[0].ID, sourceEmail.ID)
			if err := e.reconcile(ctx, candidates[0], desiredIDs, desiredNames, complete); err != nil {
				return err
			}
			return e.remember(sourceEmail, candidates[0].ID, desiredNames)
		}
	}

	e.result.Inserted++
	e.log("insert Fastmail email %q into Gmail with labels %q", sourceEmail.ID, desiredNames)
	if !e.Options.Apply {
		return nil
	}
	if !complete {
		return fmt.Errorf("cannot insert before all Gmail labels are available")
	}
	raw, err := e.Source.Raw(ctx, sourceEmail.BlobID)
	if err != nil {
		return err
	}
	insertReader, err := rewriteIdentityHeaders(raw, e.accountHash, sourceEmail.ID, messageID, firstMessageID(sourceEmail.MessageIDs) == "")
	if err != nil {
		_ = raw.Close()
		return err
	}
	message, insertErr := e.Destination.Insert(ctx, insertReader, desiredIDs)
	closeErr := raw.Close()
	if insertErr != nil {
		return insertErr
	}
	if closeErr != nil {
		return fmt.Errorf("close Fastmail message body: %w", closeErr)
	}
	return e.remember(sourceEmail, message.ID, desiredNames)
}

func (e *Engine) reconcile(ctx context.Context, message Message, desired, desiredNames []string, complete bool) error {
	if !complete {
		e.result.Modified++
		e.log("set managed labels on Gmail message %q to %q", message.ID, desiredNames)
		return nil
	}
	current := sliceSet(message.LabelIDs)
	desiredSet := sliceSet(desired)
	var add, remove []string
	for id := range desiredSet {
		if !current[id] {
			add = append(add, id)
		}
	}
	for id := range current {
		if e.managedIDs[id] && !desiredSet[id] {
			remove = append(remove, id)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(add) == 0 && len(remove) == 0 {
		e.result.Unchanged++
		return nil
	}
	e.result.Modified++
	e.log("modify Gmail message %q labels (add=%q remove=%q)", message.ID, e.labelNamesForIDs(add), e.labelNamesForIDs(remove))
	if !e.Options.Apply {
		return nil
	}
	_, err := e.Destination.ModifyLabels(ctx, message.ID, add, remove)
	return err
}

func (e *Engine) labelNamesForIDs(ids []string) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := e.labelNames[id]
		if name == "" {
			name = id
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (e *Engine) remember(sourceEmail Email, gmailID string, labels []string) error {
	if !e.Options.Apply {
		return nil
	}
	next := store.Message{GmailID: gmailID}
	if current, ok := e.state.Messages[sourceEmail.ID]; ok &&
		current.GmailID == next.GmailID &&
		!current.SourceDeleted {
		return nil
	}
	e.state.Messages[sourceEmail.ID] = next
	return e.Store.Save(e.state)
}

func (e *Engine) destroy(ctx context.Context, sourceID string) error {
	record, ok := e.state.Messages[sourceID]
	if !ok {
		return nil
	}
	if !e.Options.Delete {
		e.result.SkippedTrash++
		e.log("retain Gmail message %q because source deletion propagation is disabled", record.GmailID)
		if e.Options.Apply {
			record.SourceDeleted = true
			e.state.Messages[sourceID] = record
			return e.Store.Save(e.state)
		}
		return nil
	}
	message, err := e.Destination.Message(ctx, record.GmailID)
	if errors.Is(err, ErrNotFound) {
		e.log("forget missing Gmail message %q for destroyed Fastmail email %q", record.GmailID, sourceID)
		return e.forget(sourceID)
	}
	if err != nil {
		return err
	}
	if hasLabelID(message, LabelSpam) {
		e.result.IgnoredSpam++
		e.log("leave Gmail spam message %q unchanged for destroyed Fastmail email %q", record.GmailID, sourceID)
		return e.forget(sourceID)
	}
	e.result.Trashed++
	e.log("trash Gmail message %q for destroyed Fastmail email %q", record.GmailID, sourceID)
	if !e.Options.Apply {
		return nil
	}
	if err := e.Destination.Trash(ctx, record.GmailID); err != nil {
		return err
	}
	return e.forget(sourceID)
}

func (e *Engine) forget(sourceID string) error {
	if !e.Options.Apply {
		return nil
	}
	delete(e.state.Messages, sourceID)
	return e.Store.Save(e.state)
}

func (e *Engine) processTombstones(ctx context.Context) error {
	if !e.Options.Delete {
		return nil
	}
	for _, sourceID := range sortedMessageKeys(e.state.Messages) {
		record := e.state.Messages[sourceID]
		if record.SourceDeleted {
			if err := e.destroy(ctx, sourceID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) withoutSpamCandidates(candidates []Message) []Message {
	candidates = append([]Message(nil), candidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	result := make([]Message, 0, len(candidates))
	for _, candidate := range candidates {
		if hasLabelID(candidate, LabelSpam) {
			e.result.IgnoredSpam++
			e.log("ignore Gmail spam candidate %q", candidate.ID)
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func (e *Engine) withoutIdentityCandidates(candidates []Message) []Message {
	result := make([]Message, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AccountHeaders != nil || candidate.EmailIDHeaders != nil {
			e.log("skip Gmail adoption candidate %q because it has identity headers without the marker label", candidate.ID)
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func hasLabelID(message Message, labelID string) bool {
	return slices.Contains(message.LabelIDs, labelID)
}

func (e *Engine) reconcileDestination(ctx context.Context, destination DestinationHistory, hadSourceCursor bool) error {
	if e.state.GmailHistoryID == 0 {
		if !hadSourceCursor {
			return nil
		}
		e.log("Gmail history cursor is unavailable; reconcile all mapped messages")
		return e.reconcileAllDestination(ctx)
	}
	ids, err := destination.ChangesSince(ctx, e.state.GmailHistoryID)
	if errors.Is(err, ErrCannotCalculateDestinationChanges) {
		e.log("Gmail change history expired; reconcile all mapped messages")
		return e.reconcileAllDestination(ctx)
	}
	if err != nil {
		return err
	}
	return e.reconcileDestinationIDs(ctx, ids)
}

func (e *Engine) reconcileAllDestination(ctx context.Context) error {
	sourceIDs := make([]string, 0, len(e.state.Messages))
	for sourceID, record := range e.state.Messages {
		if !record.SourceDeleted {
			sourceIDs = append(sourceIDs, sourceID)
		}
	}
	sort.Strings(sourceIDs)
	return e.reconcileSourceIDs(ctx, sourceIDs)
}

func sortedMessageKeys(messages map[string]store.Message) []string {
	keys := make([]string, 0, len(messages))
	for key := range messages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (e *Engine) reconcileDestinationIDs(ctx context.Context, gmailIDs []string) error {
	reverse := make(map[string]string, len(e.state.Messages))
	for sourceID, record := range e.state.Messages {
		if !record.SourceDeleted {
			reverse[record.GmailID] = sourceID
		}
	}
	var sourceIDs []string
	for _, gmailID := range sortedUniqueStrings(gmailIDs) {
		if sourceID := reverse[gmailID]; sourceID != "" {
			sourceIDs = append(sourceIDs, sourceID)
		}
	}
	sort.Strings(sourceIDs)
	return e.reconcileSourceIDs(ctx, sourceIDs)
}

func (e *Engine) reconcileSourceIDs(ctx context.Context, sourceIDs []string) error {
	sourceIDs = sortedUniqueStrings(sourceIDs)
	for start := 0; start < len(sourceIDs); start += e.Options.BatchSize {
		end := min(start+e.Options.BatchSize, len(sourceIDs))
		emails, err := e.Source.Emails(ctx, sourceIDs[start:end])
		if err != nil {
			return err
		}
		sort.Slice(emails, func(i, j int) bool { return emails[i].ID < emails[j].ID })
		for _, sourceEmail := range emails {
			if err := e.syncEmail(ctx, sourceEmail); err != nil {
				return fmt.Errorf("reconcile Gmail change for Fastmail email %q: %w", sourceEmail.ID, err)
			}
		}
	}
	return nil
}

func (e *Engine) namesToIDs(names []string) ([]string, bool) {
	ids := make([]string, 0, len(names))
	complete := true
	for _, name := range names {
		id := e.labelIDs[name]
		if id == "" {
			complete = false
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, complete
}

func (e *Engine) gmailIDMappedElsewhere(sourceID, gmailID string) bool {
	for id, record := range e.state.Messages {
		if id != sourceID && record.GmailID == gmailID && !record.SourceDeleted {
			return true
		}
	}
	return false
}

func (e *Engine) log(format string, args ...any) {
	fmt.Fprintf(e.Options.Output, format+"\n", args...)
}

func firstMessageID(ids []string) string {
	for _, id := range ids {
		if value := strings.Trim(strings.TrimSpace(id), "<>"); value != "" {
			return value
		}
	}
	return ""
}

func effectiveMessageID(email Email) string {
	if messageID := firstMessageID(email.MessageIDs); messageID != "" {
		return messageID
	}
	digest := sha256.Sum256([]byte(email.ID))
	return "jmapsync." + hex.EncodeToString(digest[:16]) + "@invalid"
}

func rewriteIdentityHeaders(raw io.Reader, accountHash, sourceID, messageID string, addMessageID bool) (io.Reader, error) {
	reader := bufio.NewReader(raw)
	var header bytes.Buffer
	skipping := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read Fastmail message headers: %w", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			if addMessageID {
				fmt.Fprintf(&header, "Message-ID: <%s>\r\n", messageID)
			}
			fmt.Fprintf(&header, "%s: %s\r\n%s: %s\r\n", HeaderAccount, accountHash, HeaderEmailID, sourceID)
			header.WriteString(line)
			return io.MultiReader(bytes.NewReader(header.Bytes()), reader), nil
		}
		continuation := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if continuation {
			if !skipping {
				header.WriteString(line)
			}
		} else {
			name, _, found := strings.Cut(line, ":")
			skipping = found && (strings.EqualFold(strings.TrimSpace(name), HeaderAccount) || strings.EqualFold(strings.TrimSpace(name), HeaderEmailID))
			if !skipping {
				header.WriteString(line)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("Fastmail message has no header/body separator")
		}
	}
}

func sliceSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func uniqueStrings(values []string) []string {
	set := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !set[value] {
			set[value] = true
			result = append(result, value)
		}
	}
	return result
}

func sortedUniqueStrings(values []string) []string {
	result := uniqueStrings(values)
	sort.Strings(result)
	return result
}
