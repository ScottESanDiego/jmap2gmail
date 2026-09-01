package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"jmapsync/internal/syncer"
)

type Client struct {
	service *gmail.Service
	user    string
}

func New(ctx context.Context, httpClient *http.Client, user string) (*Client, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("Gmail HTTP client is nil")
	}
	if user == "" {
		user = "me"
	}
	service, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create Gmail service: %w", err)
	}
	return &Client{service: service, user: user}, nil
}

func (c *Client) Labels(ctx context.Context) ([]syncer.Label, error) {
	response, err := c.service.Users.Labels.List(c.user).Context(ctx).Do()
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
	label, err := c.service.Users.Labels.Create(c.user, &gmail.Label{
		Name: name, LabelListVisibility: "labelShow", MessageListVisibility: "show",
	}).Context(ctx).Do()
	if err != nil {
		return syncer.Label{}, fmt.Errorf("create Gmail label %q: %w", name, err)
	}
	return syncer.Label{ID: label.Id, Name: label.Name, Type: label.Type}, nil
}

func (c *Client) RenameLabel(ctx context.Context, id, name string) (syncer.Label, error) {
	label, err := c.service.Users.Labels.Patch(c.user, id, &gmail.Label{Name: name}).Context(ctx).Do()
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
	var result []syncer.Message
	pageToken := ""
	for {
		call := c.service.Users.Messages.List(c.user).
			Q("rfc822msgid:" + messageID).
			IncludeSpamTrash(true).
			MaxResults(500).
			Fields("messages(id),nextPageToken")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		response, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("find Gmail message-id %q: %w", messageID, err)
		}
		for _, listed := range response.Messages {
			if listed == nil || listed.Id == "" {
				continue
			}
			message, err := c.metadataMessage(ctx, listed.Id)
			if errors.Is(err, syncer.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("get Gmail message-id %q candidate: %w", messageID, err)
			}
			result = append(result, message)
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			return result, nil
		}
	}
}

func (c *Client) ManagedMessages(ctx context.Context, labelID string) ([]syncer.Message, error) {
	var result []syncer.Message
	pageToken := ""
	for {
		call := c.service.Users.Messages.List(c.user).
			LabelIds(labelID).
			IncludeSpamTrash(true).
			MaxResults(500).
			Fields("messages(id),nextPageToken")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		response, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("list Gmail marker label: %w", err)
		}
		for _, listed := range response.Messages {
			if listed == nil || listed.Id == "" {
				continue
			}
			message, err := c.metadataMessage(ctx, listed.Id)
			if errors.Is(err, syncer.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("get marked Gmail message: %w", err)
			}
			result = append(result, message)
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			return result, nil
		}
	}
}

func (c *Client) metadataMessage(ctx context.Context, id string) (syncer.Message, error) {
	message, err := c.service.Users.Messages.Get(c.user, id).
		Format("metadata").
		MetadataHeaders(syncer.HeaderAccount, syncer.HeaderEmailID, "Message-ID").
		Fields("id,labelIds,payload(headers)").
		Context(ctx).Do()
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("get Gmail message %q metadata: %w", id, err)
	}
	result := syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}
	if message.Payload == nil {
		return result, nil
	}
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
	message, err := c.service.Users.Messages.Get(c.user, id).
		Format("minimal").
		Fields("id,labelIds").
		Context(ctx).Do()
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("get Gmail message %q: %w", id, err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

func (c *Client) Insert(ctx context.Context, raw io.Reader, labelIDs []string) (syncer.Message, error) {
	message, err := c.service.Users.Messages.Insert(c.user, &gmail.Message{LabelIds: labelIDs}).
		InternalDateSource("dateHeader").
		Media(raw, googleapi.ContentType("message/rfc822")).
		Fields("id,labelIds").
		Context(ctx).Do()
	if err != nil {
		return syncer.Message{}, fmt.Errorf("insert Gmail message: %w", err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

func (c *Client) ModifyLabels(ctx context.Context, id string, add, remove []string) (syncer.Message, error) {
	message, err := c.service.Users.Messages.Modify(c.user, id, &gmail.ModifyMessageRequest{
		AddLabelIds: add, RemoveLabelIds: remove,
	}).Fields("id,labelIds").Context(ctx).Do()
	if isHTTPStatus(err, http.StatusNotFound) {
		return syncer.Message{}, fmt.Errorf("%w: Gmail message %q", syncer.ErrNotFound, id)
	}
	if err != nil {
		return syncer.Message{}, fmt.Errorf("modify Gmail message %q labels: %w", id, err)
	}
	return syncer.Message{ID: message.Id, LabelIDs: append([]string(nil), message.LabelIds...)}, nil
}

func (c *Client) Trash(ctx context.Context, id string) error {
	_, err := c.service.Users.Messages.Trash(c.user, id).Context(ctx).Do()
	if isHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("trash Gmail message %q: %w", id, err)
	}
	return nil
}

func (c *Client) CurrentHistoryID(ctx context.Context) (uint64, error) {
	profile, err := c.service.Users.GetProfile(c.user).Fields("historyId").Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("get Gmail history cursor: %w", err)
	}
	return profile.HistoryId, nil
}

func (c *Client) DestinationIdentity(ctx context.Context) (string, error) {
	profile, err := c.service.Users.GetProfile(c.user).Fields("emailAddress").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get Gmail account identity: %w", err)
	}
	if profile.EmailAddress == "" {
		return "", fmt.Errorf("Gmail profile returned an empty email address")
	}
	return profile.EmailAddress, nil
}

func (c *Client) ChangesSince(ctx context.Context, start uint64) ([]string, error) {
	set := make(map[string]bool)
	pageToken := ""
	for {
		call := c.service.Users.History.List(c.user).
			StartHistoryId(start).
			HistoryTypes("labelAdded", "labelRemoved", "messageDeleted").
			MaxResults(500).
			Fields("history(labelsAdded(message(id)),labelsRemoved(message(id)),messagesDeleted(message(id))),nextPageToken")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		response, err := call.Context(ctx).Do()
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%w: Gmail history %d", syncer.ErrCannotCalculateDestinationChanges, start)
		}
		if err != nil {
			return nil, fmt.Errorf("list Gmail history after %d: %w", start, err)
		}
		for _, history := range response.History {
			for _, change := range history.LabelsAdded {
				if change.Message != nil {
					set[change.Message.Id] = true
				}
			}
			for _, change := range history.LabelsRemoved {
				if change.Message != nil {
					set[change.Message.Id] = true
				}
			}
			for _, change := range history.MessagesDeleted {
				if change.Message != nil {
					set[change.Message.Id] = true
				}
			}
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			break
		}
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func isHTTPStatus(err error, status int) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == status
}
