package syncer

import (
	"context"
	"errors"
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
