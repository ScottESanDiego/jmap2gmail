package syncer

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"jmap2gmail/internal/store"
)

type StateStore interface {
	Load() (*store.State, error)
	Save(*store.State) error
}

type Options struct {
	IncludeMailboxes []string
	Apply            bool
	Delete           bool
	Full             bool
	RoleLabels       map[string]string
	BatchSize        int
	Output           io.Writer
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

	selectedMailboxes  map[string]bool
	scopeChanged       bool
	desiredScope       string
	state              *store.State
	mailboxNames       map[string]string
	labelIDs           map[string]string
	labelNames         map[string]string
	managedIDs         map[string]bool
	managedLabel       string
	accountHash        string
	labels             []Label
	snapshot           map[string]Email
	snapshotIDs        []string
	snapshotAt         string
	adoptions          map[string]bool
	adoptionScan       map[string][]Message
	result             Result
	inventory          []Message
	inventoryLoaded    bool
	inventoryValidated bool
	reconcileInventory map[string]Message
	inventoryIndex     map[string][]Message
	liveSourceIDs      map[string]bool
	trashHandled       map[string]bool
	spamHandled        map[string]bool
	historyBaseline    uint64
	now                func() time.Time
	saveFailed         bool
	workJournal        *store.Resume
	workIndex          map[string]int
}

func (e *Engine) Run(ctx context.Context) (Result, error) {
	if e.Source == nil || e.Destination == nil || e.Store == nil {
		return Result{}, errors.New("sync engine requires source, destination, and state store")
	}
	e.defaults()
	e.saveFailed = false
	e.workJournal = nil
	e.workIndex = nil
	e.labels = nil
	e.inventory = nil
	e.inventoryLoaded = false
	e.inventoryIndex = nil
	e.snapshot = nil
	e.snapshotIDs = nil
	e.adoptionScan = nil
	e.adoptions = nil
	e.selectedMailboxes = nil
	e.result = Result{}
	e.inventoryValidated = false
	e.reconcileInventory = nil
	defer func() { e.reconcileInventory = nil; e.inventoryValidated = false }()
	e.liveSourceIDs = make(map[string]bool)
	e.trashHandled = make(map[string]bool)
	e.spamHandled = make(map[string]bool)
	state, err := e.Store.Load()
	if err != nil {
		return Result{}, err
	}
	if !e.Options.Apply {
		state = state.Clone()
	}
	e.state = state
	if err := e.bindAccounts(ctx); err != nil {
		return Result{}, err
	}

	mailboxes, err := e.Source.Mailboxes(ctx)
	if err != nil {
		return Result{}, err
	}
	previousScope := e.state.MailboxScope
	if err := e.prepareMailboxScope(mailboxes); err != nil {
		return Result{}, err
	}
	e.desiredScope = e.state.MailboxScope
	e.state.MailboxScope = previousScope
	e.mailboxNames, err = MailboxLabelNames(mailboxes, e.Options.RoleLabels)
	if err != nil {
		return Result{}, err
	}
	if e.selectedMailboxes != nil {
		for id := range e.mailboxNames {
			if !e.selectedMailboxes[id] {
				delete(e.mailboxNames, id)
			}
		}
	}
	historyDestination, hasHistory := e.Destination.(DestinationHistory)
	var historyBaseline uint64
	if hasHistory {
		historyBaseline, err = historyDestination.CurrentHistoryID(ctx)
		if err != nil {
			return Result{}, err
		}
	}
	e.historyBaseline = historyBaseline
	return e.runResumable(ctx, mailboxes, hasHistory)
}

func (e *Engine) defaults() {
	if e.now == nil {
		e.now = time.Now
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
	e.managedLabel = "jmap2gmail-managed/" + e.accountHash
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
	e.inventoryValidated = false
	e.reconcileInventory = nil
	if err := e.loadSnapshot(ctx); err != nil {
		return err
	}
	e.log("rebuild: inventorying Gmail metadata")
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
	messages, err := e.Destination.Inventory(ctx)
	if err != nil {
		return err
	}
	e.inventoryIndex = make(map[string][]Message)
	seenInventory := make(map[string]bool, len(messages))
	nonSpamInventory := make(map[string]bool, len(messages))
	for _, message := range messages {
		if message.ID == "" || seenInventory[message.ID] {
			return errors.New("Gmail inventory has an empty or duplicate message ID")
		}
		seenInventory[message.ID] = true
		if hasLabelID(message, LabelSpam) {
			continue
		}
		nonSpamInventory[message.ID] = true
		if len(message.MessageIDHeaders) == 1 {
			id := firstMessageID(message.MessageIDHeaders)
			if id != "" {
				e.inventoryIndex[id] = append(e.inventoryIndex[id], message)
			}
		}
	}
	// A missing destination ID cannot reserve its source mapping during local
	// adoption. Present copies, including Spam and Trash, keep their mappings.
	for sourceID, record := range e.state.Messages {
		if !seenInventory[record.GmailID] {
			delete(e.state.Messages, sourceID)
		}
	}
	e.inventory = messages
	e.inventoryLoaded = true
	slices.SortFunc(messages, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	claimedGmail := make(map[string]string)
	legacy := make(map[string][]Message)
	for _, message := range messages {
		if hasLabelID(message, LabelSpam) {
			e.countIgnoredSpam(message.ID)
			e.log("ignore marked Gmail spam message %q", message.ID)
			continue
		}
		marked := markerID != "" && hasLabelID(message, markerID)
		if !marked && (len(message.AccountHeaders) != 1 || strings.TrimSpace(message.AccountHeaders[0]) != e.accountHash) {
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
		if prior := e.state.Messages[sourceID].GmailID; prior != "" && prior != message.ID && nonSpamInventory[prior] {
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
	for _, messageID := range slices.Sorted(maps.Keys(legacy)) {
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
	e.inventoryValidated = true
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
	messageIDs := slices.Sorted(maps.Keys(byMessageID))
	e.log("rebuild: matching Gmail messages by Message-ID (0/%d messages)", len(messageIDs))
	started := e.now()
	lastReport := started
	for index, messageID := range messageIDs {
		candidates := e.inventoryIndex[messageID]
		completed := index + 1
		now := e.now()
		if completed == 1 || completed%100 == 0 || now.Sub(lastReport) >= 5*time.Second || completed == len(messageIDs) {
			e.log("rebuild: matched Gmail messages by Message-ID (%d/%d messages, elapsed %s)", completed, len(messageIDs), now.Sub(started).Truncate(time.Second))
			lastReport = now
		}
		candidates = e.withoutSpamCandidates(candidates)
		candidates = e.withoutIdentityCandidates(candidates)
		sourceIDs := byMessageID[messageID]
		slices.Sort(sourceIDs)
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
	if len(messageIDs) == 0 {
		e.log("rebuild: matched Gmail messages by Message-ID (0/0 messages, elapsed %s)", e.now().Sub(started).Truncate(time.Second))
	}
	return nil
}

func (e *Engine) loadSnapshot(ctx context.Context) error {
	e.log("rebuild: inventorying Fastmail messages")
	baseline, err := e.Source.State(ctx)
	if err != nil {
		return err
	}
	var ids []string
	if e.selectedMailboxes == nil {
		ids, err = e.Source.AllEmailIDs(ctx)
	} else {
		ids, err = e.Source.(SourceMailboxQuery).EmailIDsInMailboxes(ctx, slices.Sorted(maps.Keys(e.selectedMailboxes)))
	}
	if err != nil {
		return err
	}
	slices.Sort(ids)
	if len(slices.Compact(append([]string(nil), ids...))) != len(ids) {
		return errors.New("Fastmail snapshot contains duplicate IDs; retry the sync")
	}
	snapshot := make(map[string]Email, len(ids))
	batchDone := func() { e.log("rebuild: loading Fastmail metadata (%d/%d messages)", len(snapshot), len(ids)) }
	batchDone()
	if err := e.visitEmailBatches(ctx, ids, func(email Email) error {
		snapshot[email.ID] = email
		return nil
	}, batchDone); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := snapshot[id]; !ok {
			return errors.New("Fastmail snapshot was incomplete; retry the sync")
		}
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
	for id, saved := range e.state.Mailboxes {
		if e.selectedMailboxes != nil && !e.selectedMailboxes[id] {
			continue
		}
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
	slices.SortFunc(orderedMailboxes, func(a, b Mailbox) int {
		return cmp.Compare(e.mailboxNames[a.ID], e.mailboxNames[b.ID])
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
	if e.state.Resume != nil {
		for _, id := range e.state.Resume.OldManagedLabelIDs {
			e.managedIDs[id] = true
		}
	}
	for _, name := range ManagedLabelNames(e.mailboxNames) {
		if id := e.labelIDs[name]; id != "" {
			e.managedIDs[id] = true
		}
	}
	if e.Options.Apply {
		return e.saveState()
	}
	return nil
}

func (e *Engine) reconciliationProgress(phase string, total int) func() {
	started := e.now()
	lastReport := started
	initialUnchanged := e.result.Unchanged
	completed := 0
	report := func(now time.Time) {
		e.log("reconcile: %s (%d/%d messages, unchanged %d, elapsed %s)", phase, completed, total, e.result.Unchanged-initialUnchanged, now.Sub(started).Truncate(time.Second))
		lastReport = now
	}
	report(started)
	return func() {
		completed++
		now := e.now()
		if completed == 1 || completed%100 == 0 || now.Sub(lastReport) >= 5*time.Second || completed == total {
			report(now)
		}
	}
}

func (e *Engine) syncEmail(ctx context.Context, sourceEmail Email) error {
	if !e.emailInScope(sourceEmail) {
		return e.destroy(ctx, sourceEmail.ID)
	}
	e.liveSourceIDs[sourceEmail.ID] = true
	sourceEmail = e.projectEmail(sourceEmail)
	desiredNames, err := DesiredLabelNames(sourceEmail, e.mailboxNames)
	if err != nil {
		return err
	}
	desiredNames = append(desiredNames, e.managedLabel)
	slices.Sort(desiredNames)
	desiredIDs, complete := e.namesToIDs(desiredNames)
	record, mapped := e.state.Messages[sourceEmail.ID]
	if mapped && record.GmailID != "" {
		if e.adoptions[sourceEmail.ID] {
			e.result.Adopted++
			e.log("adopt Gmail message %q for Fastmail email %q", record.GmailID, sourceEmail.ID)
			delete(e.adoptions, sourceEmail.ID)
		}
		message, reused := e.reconcileInventory[record.GmailID]
		var err error
		if !reused {
			message, err = e.Destination.Message(ctx, record.GmailID)
		}
		if err == nil {
			if hasLabelID(message, LabelSpam) {
				e.countIgnoredSpam(message.ID)
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
	if e.state.Resume != nil {
		e.state.Resume.InsertIntent = &store.InsertIntent{SourceID: sourceEmail.ID, Account: e.accountHash, MessageID: messageID}
		if err := e.checkpoint(); err != nil {
			return errors.Join(err, raw.Close())
		}
	}
	message, insertErr := e.Destination.Insert(ctx, insertReader, desiredIDs)
	if insertErr != nil {
		if e.state.Resume != nil && errors.Is(insertErr, ErrMutationRejected) {
			e.state.Resume.InsertIntent = nil
			insertErr = errors.Join(insertErr, e.checkpoint())
		}
		return errors.Join(insertErr, raw.Close())
	}
	if message.ID == "" {
		return errors.Join(errors.New("Gmail Insert returned an empty message ID; outcome requires identity recovery"), raw.Close())
	}
	// Persist the known successful ID before reporting an independent close
	// failure. Otherwise a harmless source-close error could cause a duplicate.
	e.state.Messages[sourceEmail.ID] = store.Message{GmailID: message.ID}
	if e.state.Resume != nil {
		e.state.Resume.InsertIntent = nil
	}
	saveErr := e.checkpoint()
	closeErr := raw.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close Fastmail message body: %w", closeErr)
	}
	return errors.Join(saveErr, closeErr)
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
		// Gmail does not permit clients to remove its native Sent/Draft
		// labels. A custom role override adds the user label and leaves these.
		if e.managedIDs[id] && !desiredSet[id] && id != LabelSent && id != LabelDraft {
			remove = append(remove, id)
		}
	}
	slices.Sort(add)
	slices.Sort(remove)
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
	slices.Sort(names)
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
	return e.saveState()
}

func (e *Engine) destroy(ctx context.Context, sourceID string) error {
	delete(e.liveSourceIDs, sourceID)
	record, ok := e.state.Messages[sourceID]
	if !ok || e.trashHandled[record.GmailID] {
		return nil
	}
	if !e.Options.Delete {
		e.trashHandled[record.GmailID] = true
		e.result.SkippedTrash++
		e.log("retain Gmail message %q because source deletion propagation is disabled", record.GmailID)
		if e.Options.Apply {
			record.SourceDeleted = true
			e.state.Messages[sourceID] = record
			return e.saveState()
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
		e.trashHandled[record.GmailID] = true
		e.countIgnoredSpam(message.ID)
		e.log("leave Gmail spam message %q unchanged for destroyed Fastmail email %q", record.GmailID, sourceID)
		return e.forget(sourceID)
	}
	if hasLabelID(message, LabelTrash) {
		e.trashHandled[record.GmailID] = true
		return e.retainTombstone(sourceID, record)
	}
	e.trashHandled[record.GmailID] = true
	e.result.Trashed++
	e.log("trash Gmail message %q for destroyed Fastmail email %q", record.GmailID, sourceID)
	if !e.Options.Apply {
		return nil
	}
	if err := e.Destination.Trash(ctx, record.GmailID); err != nil {
		return err
	}
	if e.selectedMailboxes != nil {
		return e.retainTombstone(sourceID, record)
	}
	return e.forget(sourceID)
}

// Keep filtered mappings so an email returning to the view restores its existing
// Gmail copy. SourceDeleted also records retained mail when deletion is disabled.
func (e *Engine) retainTombstone(sourceID string, record store.Message) error {
	if !e.Options.Apply || record.SourceDeleted {
		return nil
	}
	record.SourceDeleted = true
	e.state.Messages[sourceID] = record
	return e.saveState()
}

func (e *Engine) forget(sourceID string) error {
	if !e.Options.Apply {
		return nil
	}
	delete(e.state.Messages, sourceID)
	return e.saveState()
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
	slices.SortFunc(candidates, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	result := make([]Message, 0, len(candidates))
	for _, candidate := range candidates {
		if hasLabelID(candidate, LabelSpam) {
			e.countIgnoredSpam(candidate.ID)
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

func sortedMessageKeys(messages map[string]store.Message) []string {
	keys := make([]string, 0, len(messages))
	for key := range messages {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// visitEmails validates each complete response batch before visiting emails in
// ID order. Missing emails are allowed because they may have been deleted since
// the IDs were read; a full snapshot separately requires complete metadata.
func (e *Engine) visitEmails(ctx context.Context, ids []string, visit func(Email) error) error {
	return e.visitEmailBatches(ctx, ids, visit, nil)
}

func (e *Engine) visitEmailBatches(ctx context.Context, ids []string, visit func(Email) error, batchDone func()) error {
	ids = sortedUniqueStrings(ids)
	for batch := range slices.Chunk(ids, e.Options.BatchSize) {
		emails, err := e.Source.Emails(ctx, batch)
		if err != nil {
			return err
		}
		requested := sliceSet(batch)
		seen := make(map[string]bool, len(emails))
		for _, email := range emails {
			if email.ID == "" {
				return errors.New("Fastmail returned an email with an empty ID")
			}
			if !requested[email.ID] {
				return fmt.Errorf("Fastmail returned unrequested email %q", email.ID)
			}
			if seen[email.ID] {
				return fmt.Errorf("Fastmail returned duplicate email %q", email.ID)
			}
			seen[email.ID] = true
		}
		slices.SortFunc(emails, func(a, b Email) int { return cmp.Compare(a.ID, b.ID) })
		for _, email := range emails {
			if err := visit(email); err != nil {
				return err
			}
		}
		if batchDone != nil {
			batchDone()
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
	slices.Sort(ids)
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
	return "jmap2gmail." + hex.EncodeToString(digest[:16]) + "@invalid"
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
	slices.Sort(result)
	return result
}

func (e *Engine) countIgnoredSpam(id string) {
	if !e.spamHandled[id] {
		e.spamHandled[id] = true
		e.result.IgnoredSpam++
	}
}
