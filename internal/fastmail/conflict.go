package fastmail

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"jmap2gmail/internal/syncer"
)

// ConflictEmails requests human metadata only for failed identity matching.
func (c *Client) ConflictEmails(ctx context.Context, ids []string) (map[string]syncer.ConflictSummary, error) {
	jids := make([]jmap.ID, 0, len(ids))
	wanted := map[jmap.ID]bool{}
	for _, id := range ids {
		jids = append(jids, jmap.ID(id))
		wanted[jmap.ID(id)] = true
	}
	if len(ids) == 0 {
		return map[string]syncer.ConflictSummary{}, nil
	}
	value, err := c.invoke(ctx, &email.Get{Account: c.account, IDs: jids, Properties: []string{"id", "subject", "from", "to", "cc", "sentAt", "receivedAt", "mailboxIds"}})
	if err != nil {
		return nil, err
	}
	response, ok := value.(*email.GetResponse)
	if !ok || response.Account != c.account {
		return nil, fmt.Errorf("invalid conflict Email/get response")
	}
	result := map[string]syncer.ConflictSummary{}
	for _, item := range response.List {
		if item == nil || !wanted[item.ID] {
			return nil, fmt.Errorf("unexpected conflict Email/get item")
		}
		delete(wanted, item.ID)
		s := syncer.ConflictSummary{Subject: item.Subject, From: conflictAddresses(item.From), To: conflictAddresses(item.To), Cc: conflictAddresses(item.CC)}
		if item.SentAt != nil {
			s.SentAt = item.SentAt.Format(time.RFC3339)
		}
		if item.ReceivedAt != nil {
			s.ReceivedAt = item.ReceivedAt.Format(time.RFC3339)
		}
		for id, present := range item.MailboxIDs {
			if present {
				s.LocationIDs = append(s.LocationIDs, string(id))
			}
		}
		result[string(item.ID)] = s
	}
	return result, nil
}
func conflictAddresses(addresses []*mail.Address) string {
	var result []string
	for _, a := range addresses {
		if a != nil {
			result = append(result, a.String())
		}
	}
	return strings.Join(result, ", ")
}
