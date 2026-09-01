package gmailapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"jmapsync/internal/syncer"
)

func TestFindByMessageIDFetchesCandidateLabels(t *testing.T) {
	var requests []string
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.RequestURI())
		var body string
		switch request.URL.Path {
		case "/gmail/v1/users/me/messages":
			if got, want := request.URL.Query().Get("q"), "rfc822msgid:m1@example.com"; got != want {
				t.Errorf("query = %q, want %q", got, want)
			}
			body = `{"messages":[{"id":"g1","threadId":"t1"}]}`
		case "/gmail/v1/users/me/messages/g1":
			body = `{"id":"g1","labelIds":["INBOX","STARRED"]}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}

	service, err := gmail.NewService(context.Background(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{service: service, user: "me"}
	got, err := client.FindByMessageID(context.Background(), "<m1@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	want := []syncer.Message{{ID: "g1", LabelIDs: []string{"INBOX", "STARRED"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want list and get", requests)
	}
	if got := requests[0]; !strings.Contains(got, "includeSpamTrash=true") {
		t.Fatalf("includeSpamTrash = %q, want true", got)
	}
}

func TestFindByMessageIDPaginatesAndPreservesEveryCandidate(t *testing.T) {
	var listTokens []string
	var gets int
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/gmail/v1/users/me/messages":
			listTokens = append(listTokens, request.URL.Query().Get("pageToken"))
			if request.URL.Query().Get("includeSpamTrash") != "true" {
				t.Error("message-id list excluded spam and trash")
			}
			if request.URL.Query().Get("maxResults") != "500" {
				t.Errorf("maxResults = %q, want 500", request.URL.Query().Get("maxResults"))
			}
			if request.URL.Query().Get("fields") != "messages(id),nextPageToken" {
				t.Errorf("fields = %q", request.URL.Query().Get("fields"))
			}
			if request.URL.Query().Get("pageToken") == "" {
				body = `{"messages":[{"id":"g1"}],"nextPageToken":"page-2"}`
			} else {
				body = `{"messages":[{"id":"g2"}]}`
			}
		case "/gmail/v1/users/me/messages/g1":
			gets++
			body = `{"id":"g1","labelIds":["INBOX"]}`
		case "/gmail/v1/users/me/messages/g2":
			gets++
			body = `{"id":"g2","labelIds":["TRASH"]}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	service, err := gmail.NewService(context.Background(), option.WithEndpoint("https://gmail.test/"), option.WithHTTPClient(httpClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Client{service: service, user: "me"}).FindByMessageID(context.Background(), "m1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := []syncer.Message{{ID: "g1", LabelIDs: []string{"INBOX"}}, {ID: "g2", LabelIDs: []string{"TRASH"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(listTokens, []string{"", "page-2"}) || gets != 2 {
		t.Fatalf("list tokens = %#v, gets = %d", listTokens, gets)
	}
}

func TestManagedMessagesPaginatesAndFetchesIdentityMetadata(t *testing.T) {
	var lists, gets int
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/gmail/v1/users/me/messages":
			lists++
			if got := request.URL.Query()["labelIds"]; !reflect.DeepEqual(got, []string{"marker-id"}) {
				t.Errorf("labelIds = %#v", got)
			}
			if request.URL.Query().Get("includeSpamTrash") != "true" {
				t.Error("managed list excluded spam and trash")
			}
			if request.URL.Query().Get("pageToken") == "" {
				body = `{"messages":[{"id":"g1"}],"nextPageToken":"next"}`
			} else {
				body = `{"messages":[{"id":"g2"}]}`
			}
		case "/gmail/v1/users/me/messages/g1", "/gmail/v1/users/me/messages/g2":
			gets++
			if request.URL.Query().Get("format") != "metadata" {
				t.Errorf("format = %q", request.URL.Query().Get("format"))
			}
			headers := request.URL.Query()["metadataHeaders"]
			for _, want := range []string{syncer.HeaderAccount, syncer.HeaderEmailID, "Message-ID"} {
				if !contains(headers, want) {
					t.Errorf("metadataHeaders = %#v, missing %q", headers, want)
				}
			}
			body = `{"id":"` + request.URL.Path[len(request.URL.Path)-2:] + `","labelIds":["INBOX"],"payload":{"headers":[` +
				`{"name":"X-Jmapsync-Account","value":"account"},` +
				`{"name":"X-Jmapsync-Email-ID","value":"e1"},` +
				`{"name":"x-jmapsync-email-id","value":"e1-duplicate"},` +
				`{"name":"Message-ID","value":"<m1@example.com>"}]}}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	service, err := gmail.NewService(context.Background(), option.WithEndpoint("https://gmail.test/"), option.WithHTTPClient(httpClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := (&Client{service: service, user: "me"}).ManagedMessages(context.Background(), "marker-id")
	if err != nil {
		t.Fatal(err)
	}
	if lists != 2 || gets != 2 || len(messages) != 2 {
		t.Fatalf("lists=%d gets=%d messages=%#v", lists, gets, messages)
	}
	if got := messages[0].EmailIDHeaders; !reflect.DeepEqual(got, []string{"e1", "e1-duplicate"}) {
		t.Fatalf("duplicate identity headers = %#v", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTrashUsesGmailTrashEndpoint(t *testing.T) {
	var method, path string
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		method = request.Method
		path = request.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"g1","labelIds":["TRASH"]}`)),
			Request:    request,
		}, nil
	})}
	service, err := gmail.NewService(context.Background(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{service: service, user: "me"}
	if err := client.Trash(context.Background(), "g1"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/gmail/v1/users/me/messages/g1/trash" {
		t.Fatalf("request = %s %s, want POST trash endpoint", method, path)
	}
}

func TestChangesSinceReturnsSortedUniqueMessageIDs(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/gmail/v1/users/me/history" {
			return nil, fmt.Errorf("unexpected request: %s", request.URL)
		}
		body := `{"history":[` +
			`{"labelsAdded":[{"message":{"id":"gmail-z"}},{"message":{"id":"gmail-a"}}]},` +
			`{"labelsRemoved":[{"message":{"id":"gmail-m"}},{"message":{"id":"gmail-z"}}]},` +
			`{"messagesDeleted":[{"message":{"id":"gmail-a"}}]}` +
			`]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	service, err := gmail.NewService(context.Background(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Client{service: service, user: "me"}).ChangesSince(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gmail-a", "gmail-m", "gmail-z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message IDs = %#v, want %#v", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
