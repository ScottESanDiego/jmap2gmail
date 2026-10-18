package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"jmapsync/internal/syncer"
)

type Client struct {
	service    *gmail.Service
	user       string
	policy     *requestPolicy
	policyOnce sync.Once
}

func New(ctx context.Context, httpClient *http.Client, user string, options ...ClientOptions) (*Client, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("Gmail HTTP client is nil")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("Gmail client accepts at most one options value")
	}
	var settings ClientOptions
	if len(options) == 1 {
		settings = options[0]
	}
	policy, err := newRequestPolicy(settings)
	if err != nil {
		return nil, err
	}
	if user == "" {
		user = "me"
	}
	service, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create Gmail service: %w", err)
	}
	return &Client{service: service, user: user, policy: policy}, nil
}

func (c *Client) Labels(ctx context.Context) ([]syncer.Label, error) {
	response, err := execute(ctx, c, "labels.list", func() (*gmail.ListLabelsResponse, error) {
		return c.service.Users.Labels.List(c.user).Context(ctx).Do()
	})
	if err != nil {
		return nil, fmt.Errorf("list Gmail labels: %w", err)
	}
	result := make([]syncer.Label, 0, len(response.Labels))
	for _, label := range response.Labels {
		result = append(result, syncer.Label{ID: label.Id, Name: label.Name, Type: label.Type})
	}
	return result, nil
}

func (c *Client) CreateLabel(ctx context.Context, name string) (syncer.Label, error) {
	label, err := execute(ctx, c, "labels.create", func() (*gmail.Label, error) {
		return c.service.Users.Labels.Create(c.user, &gmail.Label{
			Name: name, LabelListVisibility: "labelShow", MessageListVisibility: "show",
		}).Context(ctx).Do()
	})
	if err != nil {
		return syncer.Label{}, fmt.Errorf("create Gmail label %q: %w", name, err)
	}
	return syncer.Label{ID: label.Id, Name: label.Name, Type: label.Type}, nil
}

func (c *Client) RenameLabel(ctx context.Context, id, name string) (syncer.Label, error) {
	label, err := execute(ctx, c, "labels.patch", func() (*gmail.Label, error) {
		return c.service.Users.Labels.Patch(c.user, id, &gmail.Label{Name: name}).Context(ctx).Do()
	})
	if err != nil {
		return syncer.Label{}, fmt.Errorf("rename Gmail label %q to %q: %w", id, name, err)
	}
	return syncer.Label{ID: label.Id, Name: label.Name, Type: label.Type}, nil
}

func (c *Client) FindByMessageID(ctx context.Context, messageID string) ([]syncer.Message, error) {
	messageID = strings.Trim(strings.TrimSpace(messageID), "<>")
	if messageID == "" {
		return nil, nil
	}
	return c.listMessages(ctx, "rfc822msgid:"+messageID)
}

// Inventory returns all Gmail messages, including Spam and Trash, with the
// identity metadata needed for reconciliation. The engine excludes native Spam.
func (c *Client) Inventory(ctx context.Context) ([]syncer.Message, error) {
	return c.listMessages(ctx, "")
}

func (c *Client) listMessages(ctx context.Context, query string) ([]syncer.Message, error) {
	var result []syncer.Message
	seenIDs := make(map[string]bool)
	seenTokens := make(map[string]bool)
	pageToken := ""
	for {
		seenTokens[pageToken] = true
		response, err := execute(ctx, c, "messages.list", func() (*gmail.ListMessagesResponse, error) {
			call := c.service.Users.Messages.List(c.user).
				IncludeSpamTrash(true).MaxResults(500).Fields("messages(id),nextPageToken")
			if query != "" {
				call = call.Q(query)
			}
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			return call.Context(ctx).Do()
		})
		if err != nil {
			return nil, fmt.Errorf("list Gmail messages: %w", err)
		}
		if response == nil {
			return nil, fmt.Errorf("list Gmail messages returned a null response")
		}
		for _, listed := range response.Messages {
			if listed == nil || strings.TrimSpace(listed.Id) == "" {
				return nil, fmt.Errorf("list Gmail messages returned a null message or blank ID")
			}
			if seenIDs[listed.Id] {
				return nil, fmt.Errorf("list Gmail messages returned duplicate ID %q", listed.Id)
			}
			seenIDs[listed.Id] = true
			message, err := c.metadataMessage(ctx, listed.Id)
			if errors.Is(err, syncer.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result = append(result, message)
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			return result, nil
		}
		if seenTokens[pageToken] {
			return nil, fmt.Errorf("list Gmail messages repeated page token %q", pageToken)
		}
	}
}

func (c *Client) metadataMessage(ctx context.Context, id string) (syncer.Message, error) {
	message, err := execute(ctx, c, "messages.get", func() (*gmail.Message, error) {
		return c.service.Users.Messages.Get(c.user, id).
			Format("metadata").
			MetadataHeaders(syncer.HeaderAccount, syncer.HeaderEmailID, "Message-ID").
			Fields("id,labelIds,payload(headers)").
			Context(ctx).Do()
	})
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("get Gmail message %q metadata: %w", id, err)
	}
	if message == nil || message.Id != id || strings.TrimSpace(message.Id) == "" {
		return syncer.Message{}, fmt.Errorf("Gmail metadata response does not match requested message %q", id)
	}
	if message.Payload == nil {
		return syncer.Message{}, fmt.Errorf("Gmail metadata response for %q is missing payload", id)
	}
	result := syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}
	for _, header := range message.Payload.Headers {
		if header == nil {
			continue
		}
		switch {
		case strings.EqualFold(header.Name, syncer.HeaderAccount):
			result.AccountHeaders = append(result.AccountHeaders, header.Value)
		case strings.EqualFold(header.Name, syncer.HeaderEmailID):
			result.EmailIDHeaders = append(result.EmailIDHeaders, header.Value)
		case strings.EqualFold(header.Name, "Message-ID"):
			result.MessageIDHeaders = append(result.MessageIDHeaders, header.Value)
		}
	}
	return result, nil
}

func (c *Client) Message(ctx context.Context, id string) (syncer.Message, error) {
	message, err := execute(ctx, c, "messages.get", func() (*gmail.Message, error) {
		return c.service.Users.Messages.Get(c.user, id).
			Format("minimal").
			Fields("id,labelIds").
			Context(ctx).Do()
	})
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("get Gmail message %q: %w", id, err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

func (c *Client) Insert(ctx context.Context, raw io.Reader, labelIDs []string) (syncer.Message, error) {
	if err := ctx.Err(); err != nil {
		return syncer.Message{}, err
	}
	// A quota response can arrive after the source stream has been consumed.
	// Spool once so every explicitly rejected attempt can replay exact MIME with
	// bounded memory; no ambiguous failure is replayed.
	spool, err := os.CreateTemp("", "jmapsync-gmail-*.eml")
	if err != nil {
		return syncer.Message{}, fmt.Errorf("create Gmail insert spool: %w", err)
	}
	defer func() { spool.Close(); os.Remove(spool.Name()) }()
	spooledSize, err := io.CopyBuffer(struct{ io.Writer }{spool}, contextReader{ctx: ctx, reader: raw}, make([]byte, 32*1024))
	if err != nil {
		return syncer.Message{}, fmt.Errorf("spool Gmail message: %w", err)
	}
	labels := slices.Clone(labelIDs)
	message, err := execute(ctx, c, "messages.insert", func() (*gmail.Message, error) {
		return c.service.Users.Messages.Insert(c.user, &gmail.Message{LabelIds: slices.Clone(labels)}).
			InternalDateSource("dateHeader").
			Media(io.NewSectionReader(spool, 0, spooledSize), googleapi.ContentType("message/rfc822"), googleapi.ChunkSize(0)).
			Fields("id,labelIds").Context(ctx).Do()
	})
	if err != nil {
		return syncer.Message{}, fmt.Errorf("insert Gmail message: %w", err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (c *Client) ModifyLabels(ctx context.Context, id string, add, remove []string) (syncer.Message, error) {
	addSnapshot, removeSnapshot := slices.Clone(add), slices.Clone(remove)
	message, err := execute(ctx, c, "messages.modify", func() (*gmail.Message, error) {
		return c.service.Users.Messages.Modify(c.user, id, &gmail.ModifyMessageRequest{AddLabelIds: slices.Clone(addSnapshot), RemoveLabelIds: slices.Clone(removeSnapshot)}).Fields("id,labelIds").Context(ctx).Do()
	})
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("modify Gmail message %q labels: %w", id, err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

func (c *Client) Trash(ctx context.Context, id string) error {
	_, err := execute(ctx, c, "messages.trash", func() (*gmail.Message, error) { return c.service.Users.Messages.Trash(c.user, id).Context(ctx).Do() })
	if isHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("trash Gmail message %q: %w", id, err)
	}
	return nil
}

func (c *Client) CurrentHistoryID(ctx context.Context) (uint64, error) {
	profile, err := execute(ctx, c, "getProfile", func() (*gmail.Profile, error) {
		return c.service.Users.GetProfile(c.user).Fields("historyId").Context(ctx).Do()
	})
	if err != nil {
		return 0, fmt.Errorf("get Gmail history cursor: %w", err)
	}
	if profile == nil || profile.HistoryId == 0 {
		return 0, fmt.Errorf("Gmail profile returned an empty history cursor")
	}
	return profile.HistoryId, nil
}

func (c *Client) DestinationIdentity(ctx context.Context) (string, error) {
	profile, err := execute(ctx, c, "getProfile", func() (*gmail.Profile, error) {
		return c.service.Users.GetProfile(c.user).Fields("emailAddress").Context(ctx).Do()
	})
	if err != nil {
		return "", fmt.Errorf("get Gmail account identity: %w", err)
	}
	if profile == nil || profile.EmailAddress == "" {
		return "", fmt.Errorf("Gmail profile returned an empty email address")
	}
	return profile.EmailAddress, nil
}

func (c *Client) ChangesSince(ctx context.Context, start uint64) ([]string, error) {
	set := make(map[string]bool)
	seenTokens := make(map[string]bool)
	pageToken := ""
	for {
		seenTokens[pageToken] = true
		response, err := execute(ctx, c, "history.list", func() (*gmail.ListHistoryResponse, error) {
			call := c.service.Users.History.List(c.user).
				StartHistoryId(start).
				HistoryTypes("messageAdded", "labelAdded", "labelRemoved", "messageDeleted").
				MaxResults(500).
				Fields("history(messagesAdded(message(id)),labelsAdded(message(id)),labelsRemoved(message(id)),messagesDeleted(message(id))),nextPageToken")
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			return call.Context(ctx).Do()
		})
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%w: Gmail history %d", syncer.ErrCannotCalculateDestinationChanges, start)
		}
		if err != nil {
			return nil, fmt.Errorf("list Gmail history after %d: %w", start, err)
		}
		if response == nil {
			return nil, fmt.Errorf("list Gmail history returned a null response")
		}
		add := func(message *gmail.Message) error {
			if message == nil || strings.TrimSpace(message.Id) == "" {
				return fmt.Errorf("Gmail history returned a null message or blank ID")
			}
			set[message.Id] = true
			return nil
		}
		for _, history := range response.History {
			if history == nil {
				return nil, fmt.Errorf("Gmail history returned a null record")
			}
			for _, change := range history.MessagesAdded {
				if change == nil {
					return nil, fmt.Errorf("Gmail history returned a null message addition")
				}
				if err := add(change.Message); err != nil {
					return nil, err
				}
			}
			for _, change := range history.LabelsAdded {
				if change == nil {
					return nil, fmt.Errorf("Gmail history returned a null label addition")
				}
				if err := add(change.Message); err != nil {
					return nil, err
				}
			}
			for _, change := range history.LabelsRemoved {
				if change == nil {
					return nil, fmt.Errorf("Gmail history returned a null label removal")
				}
				if err := add(change.Message); err != nil {
					return nil, err
				}
			}
			for _, change := range history.MessagesDeleted {
				if change == nil {
					return nil, fmt.Errorf("Gmail history returned a null message deletion")
				}
				if err := add(change.Message); err != nil {
					return nil, err
				}
			}
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			break
		}
		if seenTokens[pageToken] {
			return nil, fmt.Errorf("Gmail history repeated page token %q", pageToken)
		}
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	slices.Sort(result)
	return result, nil
}

func isHTTPStatus(err error, status int) bool {
	apiErr, ok := errors.AsType[*googleapi.Error](err)
	return ok && apiErr.Code == status
}
