package fastmail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"jmapsync/internal/syncer"
)

const (
	DefaultSessionEndpoint = "https://api.fastmail.com/jmap/session"
	defaultPageSize        = 500
	stateProbeID           = "jmapsync-state-probe"
)

type Client struct {
	client  *jmap.Client
	account jmap.ID
}

// Keep total's presence: an omitted or null total must never be mistaken for
// an authoritative empty inventory. The shallower field shadows the library's
// numeric Total while retaining its other response fields.
type queryResponse struct {
	email.QueryResponse
	Total *uint64 `json:"total"`
}

func init() {
	// The imported email package has already registered its methods. Override
	// only Email/query once during initialization, before concurrent requests.
	jmap.RegisterMethod("Email/query", func() jmap.MethodResponse { return &queryResponse{} })
}

func New(token, sessionEndpoint string) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("Fastmail API token is empty")
	}
	if sessionEndpoint == "" {
		sessionEndpoint = DefaultSessionEndpoint
	}
	raw := &jmap.Client{SessionEndpoint: sessionEndpoint}
	raw.WithAccessToken(token)
	raw.HttpClient.Timeout = 60 * time.Second
	if err := raw.Authenticate(); err != nil {
		return nil, fmt.Errorf("authenticate to Fastmail JMAP: %w", err)
	}
	account, ok := raw.Session.PrimaryAccounts[mail.URI]
	if !ok || account == "" {
		return nil, fmt.Errorf("Fastmail session has no primary JMAP Mail account")
	}
	return &Client{client: raw, account: account}, nil
}

func (c *Client) invoke(ctx context.Context, method jmap.Method) (any, error) {
	request := &jmap.Request{Context: ctx}
	request.Invoke(method)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	if len(response.Responses) != 1 {
		return nil, fmt.Errorf("JMAP returned %d method responses, want 1", len(response.Responses))
	}
	if methodErr, ok := response.Responses[0].Args.(*jmap.MethodError); ok {
		return nil, methodErr
	}
	return response.Responses[0].Args, nil
}

func (c *Client) Mailboxes(ctx context.Context) ([]syncer.Mailbox, error) {
	value, err := c.invoke(ctx, &mailbox.Get{Account: c.account})
	if err != nil {
		return nil, fmt.Errorf("Mailbox/get: %w", err)
	}
	response, ok := value.(*mailbox.GetResponse)
	if !ok {
		return nil, fmt.Errorf("Mailbox/get returned %T", value)
	}
	result := make([]syncer.Mailbox, 0, len(response.List))
	for _, item := range response.List {
		result = append(result, syncer.Mailbox{
			ID: string(item.ID), Name: item.Name,
			ParentID: string(item.ParentID), Role: string(item.Role),
		})
	}
	return result, nil
}

func (c *Client) SourceIdentity(context.Context) (string, error) {
	return c.client.Session.Username + "|" + string(c.account), nil
}

func (c *Client) State(ctx context.Context) (string, error) {
	value, err := c.invoke(ctx, &email.Get{
		Account:    c.account,
		IDs:        []jmap.ID{jmap.ID(stateProbeID)},
		Properties: []string{"id"},
	})
	if err != nil {
		return "", fmt.Errorf("Email/get state: %w", err)
	}
	response, ok := value.(*email.GetResponse)
	if !ok {
		return "", fmt.Errorf("Email/get state returned %T", value)
	}
	if response.State == "" {
		return "", fmt.Errorf("Email/get state returned an empty state")
	}
	return response.State, nil
}

func (c *Client) AllEmailIDs(ctx context.Context) ([]string, error) {
	return c.emailIDs(ctx, nil)
}

// EmailIDsInMailboxes queries the union of exactly these mailbox IDs. Empty
// selections return no messages rather than accidentally querying all mail.
func (c *Client) EmailIDsInMailboxes(ctx context.Context, ids []string) ([]string, error) {
	conditions := make([]email.Filter, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("cannot query an empty mailbox ID")
		}
		if !seen[id] {
			conditions = append(conditions, &email.FilterCondition{InMailbox: jmap.ID(id)})
			seen[id] = true
		}
	}
	if len(conditions) == 0 {
		return []string{}, nil
	}
	return c.emailIDs(ctx, &email.FilterOperator{Operator: jmap.OperatorOR, Conditions: conditions})
}

func (c *Client) emailIDs(ctx context.Context, filter email.Filter) ([]string, error) {
	for attempt := 1; attempt <= 5; attempt++ {
		result, stable, err := c.allEmailIDsAttempt(ctx, filter)
		if err != nil {
			return nil, err
		}
		if stable {
			return result, nil
		}
	}
	return nil, fmt.Errorf("Fastmail Email/query changed during 5 consecutive pagination attempts")
}

func (c *Client) allEmailIDsAttempt(ctx context.Context, filter email.Filter) ([]string, bool, error) {
	var result []string
	var queryState string
	var total uint64
	seen := make(map[jmap.ID]bool)
	for position := int64(0); ; {
		value, err := c.invoke(ctx, &email.Query{
			Account: c.account, Position: position, Limit: defaultPageSize,
			CalculateTotal: true, Filter: filter,
		})
		if err != nil {
			return nil, false, fmt.Errorf("Email/query at position %d: %w", position, err)
		}
		response, ok := value.(*queryResponse)
		if !ok {
			return nil, false, fmt.Errorf("Email/query returned %T", value)
		}
		if response.Account != c.account || response.QueryState == "" || response.IDs == nil || response.Total == nil || response.Position != uint64(position) {
			return nil, false, fmt.Errorf("Email/query returned incomplete or misplaced inventory")
		}
		if queryState == "" {
			queryState, total = response.QueryState, *response.Total
		} else if response.QueryState != queryState {
			return nil, false, nil
		} else if *response.Total != total {
			return nil, false, fmt.Errorf("Email/query total changed without queryState change")
		}
		for _, id := range response.IDs {
			if id == "" || seen[id] {
				return nil, false, fmt.Errorf("Email/query returned empty or duplicate ID")
			}
			result = append(result, string(id))
			seen[id] = true
		}
		position += int64(len(response.IDs))
		if uint64(position) > total {
			return nil, false, fmt.Errorf("Email/query returned more IDs than total")
		}
		if uint64(position) == total {
			return result, true, nil
		}
		if len(response.IDs) == 0 {
			return nil, false, fmt.Errorf("Email/query ended before total")
		}
	}
}

func (c *Client) Emails(ctx context.Context, ids []string) ([]syncer.Email, error) {
	result := make([]syncer.Email, 0, len(ids))
	for start := 0; start < len(ids); start += defaultPageSize {
		end := min(start+defaultPageSize, len(ids))
		jmapIDs := make([]jmap.ID, 0, end-start)
		for _, id := range ids[start:end] {
			jmapIDs = append(jmapIDs, jmap.ID(id))
		}
		requested := make(map[jmap.ID]bool, len(jmapIDs))
		for _, id := range jmapIDs {
			if id == "" || requested[id] {
				return nil, fmt.Errorf("Email/get requested empty or duplicate ID %q", id)
			}
			requested[id] = true
		}
		value, err := c.invoke(ctx, &email.Get{
			Account: c.account,
			IDs:     jmapIDs,
			Properties: []string{
				"id", "blobId", "mailboxIds", "keywords", "messageId", "size",
			},
		})
		if err != nil {
			return nil, fmt.Errorf("Email/get: %w", err)
		}
		response, ok := value.(*email.GetResponse)
		if !ok {
			return nil, fmt.Errorf("Email/get returned %T", value)
		}
		if response.Account != c.account {
			return nil, fmt.Errorf("Email/get returned wrong account")
		}
		for _, id := range response.NotFound {
			if !requested[id] {
				return nil, fmt.Errorf("Email/get returned unexpected or duplicate notFound ID %q", id)
			}
			delete(requested, id)
		}
		for _, item := range response.List {
			if item == nil || !requested[item.ID] {
				return nil, fmt.Errorf("Email/get returned unexpected, duplicate, or null email")
			}
			delete(requested, item.ID)
			mailboxIDs := make(map[string]bool, len(item.MailboxIDs))
			for id, present := range item.MailboxIDs {
				mailboxIDs[string(id)] = present
			}
			keywords := make(map[string]bool, len(item.Keywords))
			maps.Copy(keywords, item.Keywords)
			result = append(result, syncer.Email{
				ID: string(item.ID), BlobID: string(item.BlobID),
				MailboxIDs: mailboxIDs, Keywords: keywords,
				MessageIDs: append([]string(nil), item.MessageID...), Size: item.Size,
			})
		}
		if len(requested) != 0 {
			return nil, fmt.Errorf("Email/get did not account for %d requested IDs", len(requested))
		}
	}
	return result, nil
}

func (c *Client) Changes(ctx context.Context, since string) (syncer.Changes, error) {
	value, err := c.invoke(ctx, &email.Changes{
		Account: c.account, SinceState: since, MaxChanges: defaultPageSize,
	})
	if err != nil {
		if methodErr, ok := errors.AsType[*jmap.MethodError](err); ok && methodErr.Type == "cannotCalculateChanges" {
			return syncer.Changes{}, fmt.Errorf("%w: %v", syncer.ErrCannotCalculateChanges, err)
		}
		return syncer.Changes{}, fmt.Errorf("Email/changes: %w", err)
	}
	response, ok := value.(*email.ChangesResponse)
	if !ok {
		return syncer.Changes{}, fmt.Errorf("Email/changes returned %T", value)
	}
	return syncer.Changes{
		NewState:  response.NewState,
		More:      response.HasMoreChanges,
		Created:   idsToStrings(response.Created),
		Updated:   idsToStrings(response.Updated),
		Destroyed: idsToStrings(response.Destroyed),
	}, nil
}

func idsToStrings(ids []jmap.ID) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		result = append(result, string(id))
	}
	return result
}

func (c *Client) Raw(ctx context.Context, blobID string) (io.ReadCloser, error) {
	reader, err := c.client.DownloadWithContext(ctx, c.account, jmap.ID(blobID))
	if err != nil {
		return nil, fmt.Errorf("download Fastmail blob %q: %w", blobID, err)
	}
	return reader, nil
}
