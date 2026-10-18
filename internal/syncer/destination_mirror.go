package syncer

import (
	"context"
	"errors"
	"fmt"
)

func (e *Engine) destinationNeedsInventory(ctx context.Context, destination DestinationHistory) (bool, error) {
	if e.state.GmailHistoryID == 0 {
		return true, nil
	}
	ids, err := destination.ChangesSince(ctx, e.state.GmailHistoryID)
	if errors.Is(err, ErrCannotCalculateDestinationChanges) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return e.unknownDestinationChanges(ctx, ids)
}

func (e *Engine) unknownDestinationChanges(ctx context.Context, ids []string) (bool, error) {
	known := make(map[string]bool, len(e.state.Messages))
	for _, record := range e.state.Messages {
		known[record.GmailID] = true
	}
	for _, id := range sortedUniqueStrings(ids) {
		if known[id] || e.trashHandled[id] {
			continue
		}
		message, err := e.Destination.Message(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if hasLabelID(message, LabelSpam) || hasLabelID(message, LabelTrash) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (e *Engine) cleanupExtras(ctx context.Context) error {
	protected := make(map[string]bool)
	for sourceID := range e.liveSourceIDs {
		if record, ok := e.state.Messages[sourceID]; ok {
			protected[record.GmailID] = true
		}
	}
	for _, listed := range e.inventory {
		if protected[listed.ID] || e.trashHandled[listed.ID] || hasLabelID(listed, LabelSpam) {
			continue
		}
		message, err := e.Destination.Message(ctx, listed.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if hasLabelID(message, LabelSpam) || hasLabelID(message, LabelTrash) {
			continue
		}
		e.trashHandled[message.ID] = true
		if !e.Options.Delete {
			e.result.SkippedTrash++
			e.log("retain Gmail message %q absent from Fastmail source view because deletion propagation is disabled", message.ID)
			continue
		}
		e.result.Trashed++
		e.log("trash Gmail message %q absent from Fastmail source view", message.ID)
		if e.Options.Apply {
			if err := e.Destination.Trash(ctx, message.ID); err != nil {
				return fmt.Errorf("trash extra Gmail message %q: %w", message.ID, err)
			}
		}
	}
	return nil
}
