package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"jmap2gmail/internal/store"
)

// A checkpoint bounds replay of idempotent work. Insert has its own immediate
// write-ahead checkpoint because a lost response cannot safely be retried.
const checkpointBatch = 100

func (e *Engine) checkpoint() error {
	if !e.Options.Apply {
		return nil
	}
	if err := e.Store.Save(e.state); err != nil {
		e.saveFailed = true
		return err
	}
	return nil
}

// Existing reconciliation helpers update the current journal in memory. Its
// owner checkpoints batches, phase boundaries, and controlled error exits.
func (e *Engine) saveState() error {
	if e.state.Resume != nil {
		return nil
	}
	return e.checkpoint()
}

func sourceRecord(email Email) store.SourceEmail {
	memberships := maps.Clone(email.MailboxIDs)
	if memberships == nil {
		memberships = map[string]bool{}
	}
	keywords := maps.Clone(email.Keywords)
	if keywords == nil {
		keywords = map[string]bool{}
	}
	return store.SourceEmail{ID: email.ID, BlobID: email.BlobID, MailboxIDs: memberships, Keywords: keywords, MessageIDs: slices.Clone(email.MessageIDs), Size: email.Size}
}
func sourceEmail(record store.SourceEmail) Email {
	return Email{ID: record.ID, BlobID: record.BlobID, MailboxIDs: maps.Clone(record.MailboxIDs), Keywords: maps.Clone(record.Keywords), MessageIDs: slices.Clone(record.MessageIDs), Size: record.Size}
}
func (e *Engine) policyFingerprint() string {
	encoded, _ := json.Marshal(struct {
		Labels map[string]string
		Delete bool
	}{e.mailboxNames, e.Options.Delete})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) runResumable(ctx context.Context, mailboxes []Mailbox, hasHistory bool) (result Result, runErr error) {
	// A failed checkpoint must never be overwritten by a best-effort error save:
	// the on-disk Insert intent may be the only evidence of a remote success.
	defer func() {
		e.reconcileInventory = nil
		if runErr != nil && e.state.Resume != nil && !e.saveFailed {
			runErr = errors.Join(runErr, e.checkpoint())
		}
		result = e.result
	}()
	policy := e.policyFingerprint()
	policyChanged := e.state.PolicyFingerprint != "" && e.state.PolicyFingerprint != policy
	if e.state.PolicyFingerprint == "" {
		// Legacy clean caches have no fingerprint. Saved effective label names
		// still reveal policy changes that require reevaluating completed mail.
		for id, saved := range e.state.Mailboxes {
			if name, ok := e.mailboxNames[id]; ok && saved.Name != name {
				policyChanged = true
				break
			}
		}
	}
	resuming := e.state.Resume != nil
	intent := (*store.InsertIntent)(nil)
	if resuming {
		intent = e.state.Resume.InsertIntent
	}
	needsFull := e.Options.Full || e.scopeChanged || e.state.Rebuild || (!resuming && e.state.EmailState == "") || !hasHistory || intent != nil
	if policyChanged && !e.state.SourceEmailsComplete {
		needsFull = true
	}
	if !needsFull && !resuming && hasHistory {
		var err error
		needsFull, err = e.destinationNeedsInventory(ctx, e.Destination.(DestinationHistory))
		if err != nil {
			return e.result, err
		}
	}
	if needsFull {
		if err := e.planFull(ctx, policy, intent); err != nil {
			return e.result, err
		}
		resuming = false
	} else if !resuming {
		if e.state.SourceEmails == nil {
			e.state.SourceEmails = map[string]store.SourceEmail{}
		}
		e.state.Resume = &store.Resume{Version: store.ResumeVersion, Mode: "incremental", Phase: "labels", SourceCursor: e.state.EmailState, GmailHistoryID: e.state.GmailHistoryID}
		e.state.Incomplete = true
		if e.state.PolicyFingerprint == "" {
			e.state.PolicyFingerprint = policy
		}
		if err := e.collectSourceChanges(ctx); err != nil {
			if !errors.Is(err, ErrCannotCalculateChanges) {
				return e.result, err
			}
			if err := e.planFull(ctx, policy, nil); err != nil {
				return e.result, err
			}
		}
		for id, record := range e.state.Messages {
			if e.Options.Delete && record.SourceDeleted && !e.hasQueuedWork(id) {
				e.queueWork(id, "destroy")
			}
		}
		if err := e.checkpoint(); err != nil {
			return e.result, err
		}
	}
	if policyChanged && e.state.SourceEmailsComplete {
		r := e.state.Resume
		for id, mailbox := range e.state.Mailboxes {
			if e.selectedMailboxes != nil && !e.selectedMailboxes[id] {
				continue
			}
			r.OldManagedLabelIDs = append(r.OldManagedLabelIDs, mailbox.GmailLabelID)
		}
		r.OldManagedLabelIDs = sortedUniqueStrings(r.OldManagedLabelIDs)
		for _, id := range slices.Sorted(maps.Keys(e.state.SourceEmails)) {
			e.queueWork(id, "reconcile")
		}
		r.Phase = "labels"
		e.state.PolicyFingerprint = policy
	}
	// Every resumed plan observes changes before replaying old destroys or global
	// cleanup. An expired boundary or new unrelated Gmail message needs preflight.
	if resuming {
		if err := e.catchup(ctx); err != nil {
			if !errors.Is(err, ErrCannotCalculateChanges) && !errors.Is(err, ErrCannotCalculateDestinationChanges) {
				return e.result, err
			}
			if err := e.planFull(ctx, policy, nil); err != nil {
				return e.result, err
			}
		}
		e.state.Resume.Phase = "labels"
		if err := e.checkpoint(); err != nil {
			return e.result, err
		}
	}
	if err := e.resolveLabels(ctx, e.selectedMailboxList(mailboxes)); err != nil {
		return e.result, err
	}
	r := e.state.Resume
	r.Phase = "source"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.processResumeWork(ctx, true); err != nil {
		return e.result, err
	}
	r.Phase = "catchup"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.catchup(ctx); err != nil {
		if !errors.Is(err, ErrCannotCalculateChanges) && !errors.Is(err, ErrCannotCalculateDestinationChanges) {
			return e.result, err
		}
		// Re-enter through a fresh full plan; never clean up using an expired view.
		if err := e.planFull(ctx, policy, nil); err != nil {
			return e.result, err
		}
		return e.continueFullPlan(ctx, mailboxes)
	}
	r = e.state.Resume
	r.Phase = "caught-up"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.processResumeWork(ctx, false); err != nil {
		return e.result, err
	}
	return e.finishResume(ctx, mailboxes)
}

func (e *Engine) continueFullPlan(ctx context.Context, mailboxes []Mailbox) (Result, error) {
	if err := e.resolveLabels(ctx, e.selectedMailboxList(mailboxes)); err != nil {
		return e.result, err
	}
	e.state.Resume.Phase = "source"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.processResumeWork(ctx, true); err != nil {
		return e.result, err
	}
	e.state.Resume.Phase = "catchup"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.catchup(ctx); err != nil {
		return e.result, err
	}
	e.state.Resume.Phase = "caught-up"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if err := e.processResumeWork(ctx, false); err != nil {
		return e.result, err
	}
	return e.finishResume(ctx, mailboxes)
}

func (e *Engine) planFull(ctx context.Context, policy string, intent *store.InsertIntent) error {
	original := e.state
	e.state = original.Clone()
	planned := false
	defer func() {
		if !planned {
			e.state = original
			e.inventoryValidated = false
			e.reconcileInventory = nil
		}
	}()

	if e.state.Rebuild {
		e.log("state cache %s; rebuilding from Fastmail and Gmail", e.state.RebuildReason)
	}
	if err := e.preflightRebuild(ctx); err != nil {
		return err
	}
	if intent != nil {
		matches := 0
		for _, message := range e.inventory {
			if len(message.AccountHeaders) == 1 && len(message.EmailIDHeaders) == 1 && strings.TrimSpace(message.AccountHeaders[0]) == intent.Account && strings.TrimSpace(message.EmailIDHeaders[0]) == intent.SourceID {
				matches++
				e.state.Messages[intent.SourceID] = store.Message{GmailID: message.ID}
			}
		}
		if matches != 1 {
			return fmt.Errorf("unresolved Gmail Insert for Fastmail email %q: complete inventory found %d exact identity matches; refusing to resend or trash mail; retry after verifying the Gmail outcome", intent.SourceID, matches)
		}
	}
	records := make(map[string]store.SourceEmail, len(e.snapshot))
	work := make([]store.WorkItem, 0, len(e.snapshot)+len(e.state.Messages))
	for _, id := range e.snapshotIDs {
		records[id] = sourceRecord(e.snapshot[id])
		work = append(work, store.WorkItem{SourceID: id, Kind: "reconcile"})
	}
	for _, id := range sortedMessageKeys(e.state.Messages) {
		if _, ok := records[id]; !ok {
			work = append(work, store.WorkItem{SourceID: id, Kind: "destroy"})
		}
	}
	extras := make([]string, 0, len(e.inventory))
	for _, message := range e.inventory {
		extras = append(extras, message.ID)
	}
	oldManaged := []string{}
	if original.Resume != nil && !e.scopeChanged {
		oldManaged = append(oldManaged, original.Resume.OldManagedLabelIDs...)
	}
	for id, mailbox := range original.Mailboxes {
		if e.selectedMailboxes != nil && !e.selectedMailboxes[id] {
			continue
		}
		if mailbox.GmailLabelID != "" {
			oldManaged = append(oldManaged, mailbox.GmailLabelID)
		}
	}
	e.state.MailboxScope = e.desiredScope
	e.state.SourceEmails = records
	e.state.SourceEmailsComplete = true
	e.state.PolicyFingerprint = policy
	e.state.Incomplete = true
	e.state.Resume = &store.Resume{Version: store.ResumeVersion, Mode: "full", Phase: "labels", SourceCursor: e.snapshotAt, GmailHistoryID: e.historyBaseline, Work: work, ExtraIDs: extras, OldManagedLabelIDs: sortedUniqueStrings(oldManaged)}
	e.state.Rebuild = false
	e.state.RebuildReason = ""
	*original = *e.state
	e.state = original
	planned = true
	return e.checkpoint()
}

func (e *Engine) hasQueuedWork(id string) bool {
	e.compactWork()
	_, ok := e.workIndex[id]
	return ok
}

func (e *Engine) compactWork() {
	r := e.state.Resume
	if r.Next > 0 {
		r.Work = append([]store.WorkItem(nil), r.Work[r.Next:]...)
		r.Next = 0
		e.workJournal = nil
	}
	if e.workJournal != r {
		e.workJournal = r
		e.workIndex = make(map[string]int, len(r.Work))
		for i, item := range r.Work {
			e.workIndex[item.SourceID] = i
		}
	}
}

// queueWork compacts once and indexes pending items, keeping bulk changes
// linear while replacing superseded work without duplicate journal IDs.
func (e *Engine) queueWork(id, kind string) {
	e.compactWork()
	r := e.state.Resume
	if index, ok := e.workIndex[id]; ok {
		r.Work[index].Kind = kind
		return
	}
	e.workIndex[id] = len(r.Work)
	r.Work = append(r.Work, store.WorkItem{SourceID: id, Kind: kind})
}

func (e *Engine) collectSourceChanges(ctx context.Context) error {
	r := e.state.Resume
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		since := r.SourceCursor
		if seen[since] {
			return errors.New("Fastmail changes cursor did not advance")
		}
		seen[since] = true
		changes, err := e.Source.Changes(ctx, since)
		if err != nil {
			return err
		}
		if changes.NewState == "" || (changes.More && changes.NewState == since) {
			return errors.New("Fastmail changes returned an invalid cursor")
		}

		ids := sortedUniqueStrings(append(slices.Clone(changes.Created), changes.Updated...))
		fetched := map[string]Email{}
		if err := e.visitEmails(ctx, ids, func(email Email) error { fetched[email.ID] = email; return nil }); err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := fetched[id]; !ok {
				return fmt.Errorf("Fastmail changed metadata is incomplete for %q: %w", id, ErrCannotCalculateChanges)
			}
		}
		e.compactWork()
		for _, id := range sortedUniqueStrings(changes.Destroyed) {
			delete(e.state.SourceEmails, id)
			e.queueWork(id, "destroy")
		}
		for _, id := range ids {
			e.state.SourceEmails[id] = sourceRecord(fetched[id])
			e.queueWork(id, "reconcile")
		}

		r.SourceCursor = changes.NewState
		if !changes.More {
			return nil
		}
	}
}

func (e *Engine) catchup(ctx context.Context) error {
	if err := e.collectSourceChanges(ctx); err != nil {
		return err
	}
	destination, ok := e.Destination.(DestinationHistory)
	if !ok {
		return nil
	}
	r := e.state.Resume
	boundary, err := destination.CurrentHistoryID(ctx)
	if err != nil {
		return err
	}
	ids, err := destination.ChangesSince(ctx, r.GmailHistoryID)
	if err != nil {
		return err
	}
	candidates := sliceSet(r.ExtraIDs)
	var unaccounted []string
	for _, id := range ids {
		if !candidates[id] {
			unaccounted = append(unaccounted, id)
		}
	}
	unknown, err := e.unknownDestinationChanges(ctx, unaccounted)
	if err != nil {
		return err
	}
	if unknown {
		return ErrCannotCalculateDestinationChanges
	}
	reverse := map[string]string{}
	for sourceID, record := range e.state.Messages {
		reverse[record.GmailID] = sourceID
	}
	var sourceIDs []string
	for _, id := range ids {
		if sourceID := reverse[id]; sourceID != "" {
			if _, live := e.state.SourceEmails[sourceID]; !live && e.state.SourceEmailsComplete {
				continue
			}
			sourceIDs = append(sourceIDs, sourceID)
		}
	}
	fetched := map[string]Email{}
	if err := e.visitEmails(ctx, sourceIDs, func(email Email) error { fetched[email.ID] = email; return nil }); err != nil {
		return err
	}
	for _, id := range sortedUniqueStrings(sourceIDs) {
		if _, ok := fetched[id]; !ok {
			return fmt.Errorf("Fastmail metadata for Gmail history is incomplete for %q: %w", id, ErrCannotCalculateChanges)
		}
	}
	for _, id := range sortedUniqueStrings(sourceIDs) {
		e.state.SourceEmails[id] = sourceRecord(fetched[id])
		e.queueWork(id, "reconcile")
	}
	r.GmailHistoryID = boundary
	return nil
}

func (e *Engine) processResumeWork(ctx context.Context, reuse bool) error {
	r := e.state.Resume
	if reuse {
		// Source reconciliation runs first; every destructive item waits for the
		// source/history catchup barrier, including moves outside the view.
		pending := r.Work[r.Next:]
		ordered := make([]store.WorkItem, 0, len(pending))
		for _, item := range pending {
			if item.Kind != "destroy" && e.emailInScope(sourceEmail(e.state.SourceEmails[item.SourceID])) {
				ordered = append(ordered, item)
			}
		}
		for _, item := range pending {
			if item.Kind == "destroy" || !e.emailInScope(sourceEmail(e.state.SourceEmails[item.SourceID])) {
				ordered = append(ordered, item)
			}
		}
		r.Work = ordered
		r.Next = 0
		e.workJournal = nil
	}
	if reuse && e.inventoryValidated {
		e.reconcileInventory = map[string]Message{}
		for _, message := range e.inventory {
			message.LabelIDs = slices.Clone(message.LabelIDs)
			e.reconcileInventory[message.ID] = message
		}
	}
	e.inventoryValidated = false
	defer func() { e.reconcileInventory = nil }()
	progress := func() {}
	if reuse && r.Mode == "full" {
		total := 0
		for _, item := range r.Work[r.Next:] {
			if item.Kind != "destroy" && e.emailInScope(sourceEmail(e.state.SourceEmails[item.SourceID])) {
				total++
			}
		}
		progress = e.reconciliationProgress("full Fastmail snapshot", total)
	}
	for r.Next < len(r.Work) {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := r.Work[r.Next]
		record, hasMetadata := e.state.SourceEmails[item.SourceID]
		live := hasMetadata && e.emailInScope(sourceEmail(record))
		if reuse && (item.Kind == "destroy" || !live) {
			break
		}
		var err error
		if item.Kind == "destroy" && !live {
			e.reconcileInventory = nil
			err = e.destroy(ctx, item.SourceID)
		} else if hasMetadata {
			err = e.syncEmail(ctx, sourceEmail(record))
		} else {
			return fmt.Errorf("resume work for %q lacks source metadata; refusing mutation", item.SourceID)
		}
		if err != nil {
			return fmt.Errorf("sync Fastmail email %q: %w", item.SourceID, err)
		}
		r.Next++
		progress()
		if r.Next%checkpointBatch == 0 {
			if err := e.checkpoint(); err != nil {
				return err
			}
		}
	}
	return e.checkpoint()
}

func (e *Engine) finishResume(ctx context.Context, mailboxes []Mailbox) (Result, error) {
	r := e.state.Resume
	r.Phase = "extras"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	protected := map[string]bool{}
	for id, record := range e.state.SourceEmails {
		if e.emailInScope(sourceEmail(record)) {
			protected[e.state.Messages[id].GmailID] = true
		}
	}
	for r.ExtraNext < len(r.ExtraIDs) {
		if err := ctx.Err(); err != nil {
			return e.result, err
		}
		id := r.ExtraIDs[r.ExtraNext]
		if !protected[id] && !e.trashHandled[id] {
			message, err := e.Destination.Message(ctx, id)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return e.result, err
			}
			if err == nil && !hasLabelID(message, LabelSpam) && !hasLabelID(message, LabelTrash) {
				if !e.Options.Delete {
					e.result.SkippedTrash++
					e.log("retain Gmail message %q absent from Fastmail source view because deletion propagation is disabled", id)
				} else {
					e.result.Trashed++
					e.log("trash Gmail message %q absent from Fastmail source view", id)
					if e.Options.Apply {
						if err := e.Destination.Trash(ctx, id); err != nil {
							return e.result, err
						}
					}
				}
			}
		}
		r.ExtraNext++
		if r.ExtraNext%checkpointBatch == 0 {
			if err := e.checkpoint(); err != nil {
				return e.result, err
			}
		}
	}
	r.Phase = "commit"
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	e.state.EmailState = r.SourceCursor
	e.state.GmailHistoryID = r.GmailHistoryID
	current := map[string]bool{}
	for _, mailbox := range mailboxes {
		current[mailbox.ID] = true
	}
	for id := range e.state.Mailboxes {
		if !current[id] {
			delete(e.state.Mailboxes, id)
		}
	}
	e.state.Resume = nil
	e.state.Incomplete = false
	e.state.Rebuild = false
	e.state.RebuildReason = ""
	if err := e.checkpoint(); err != nil {
		return e.result, err
	}
	if !e.Options.Apply {
		e.log("leave Gmail and state unchanged")
	}
	return e.result, nil
}
