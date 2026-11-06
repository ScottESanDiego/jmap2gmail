package gmailapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"

	"jmap2gmail/internal/syncer"
)

// ConflictMessage reads selected headers, never bodies, for a failed adoption.
func (c *Client) ConflictMessage(ctx context.Context, id string) (syncer.ConflictSummary, error) {
	message, err := execute(ctx, c, "messages.get", func(attemptCtx context.Context) (*gmail.Message, error) {
		return c.service.Users.Messages.Get(c.user, id).Format("metadata").MetadataHeaders("Subject", "From", "To", "Cc", "Date").Fields("id,threadId,labelIds,internalDate,payload(headers)").Context(attemptCtx).Do()
	})
	if err != nil {
		return syncer.ConflictSummary{}, err
	}
	if message == nil || message.Id != id || message.Payload == nil {
		return syncer.ConflictSummary{}, fmt.Errorf("conflict Gmail metadata unavailable")
	}
	s := syncer.ConflictSummary{LocationIDs: message.LabelIds, ThreadID: message.ThreadId}
	if message.InternalDate != 0 {
		s.ReceivedAt = time.UnixMilli(message.InternalDate).UTC().Format(time.RFC3339)
	}
	for _, h := range message.Payload.Headers {
		if h == nil {
			continue
		}
		var field *string
		switch strings.ToLower(h.Name) {
		case "subject":
			field = &s.Subject
		case "from":
			field = &s.From
		case "to":
			field = &s.To
		case "cc":
			field = &s.Cc
		case "date":
			field = &s.SentAt
		}
		if field != nil {
			if *field != "" {
				*field += "; "
			}
			*field += h.Value
		}
	}
	return s, nil
}
