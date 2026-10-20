package gmailapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"jmap2gmail/internal/syncer"
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
			body = `{"id":"g1","labelIds":["INBOX","STARRED"],"payload":{}}`
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

	service, err := gmail.NewService(t.Context(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{service: service, user: "me"}
	got, err := client.FindByMessageID(t.Context(), "<m1@example.com>")
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
			body = `{"id":"g1","labelIds":["INBOX"],"payload":{}}`
		case "/gmail/v1/users/me/messages/g2":
			gets++
			body = `{"id":"g2","labelIds":["TRASH"],"payload":{}}`
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
	service, err := gmail.NewService(t.Context(), option.WithEndpoint("https://gmail.test/"), option.WithHTTPClient(httpClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Client{service: service, user: "me"}).FindByMessageID(t.Context(), "m1@example.com")
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

func TestInventoryPaginatesAndFetchesIdentityMetadata(t *testing.T) {
	var lists, gets int
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/gmail/v1/users/me/messages":
			lists++
			if request.URL.Query().Get("labelIds") != "" || request.URL.Query().Get("q") != "" {
				t.Error("inventory must not filter messages")
			}
			if request.URL.Query().Get("includeSpamTrash") != "true" {
				t.Error("inventory list excluded spam and trash")
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
				if !slices.Contains(headers, want) {
					t.Errorf("metadataHeaders = %#v, missing %q", headers, want)
				}
			}
			body = `{"id":"` + request.URL.Path[len(request.URL.Path)-2:] + `","labelIds":["INBOX"],"payload":{"headers":[` +
				`{"name":"X-Jmap2gmail-Account","value":"account"},` +
				`{"name":"X-Jmap2gmail-Email-ID","value":"e1"},` +
				`{"name":"x-jmap2gmail-email-id","value":"e1-duplicate"},` +
				`{"name":"Message-ID","value":"<m1@example.com>"}]}}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	service, err := gmail.NewService(t.Context(), option.WithEndpoint("https://gmail.test/"), option.WithHTTPClient(httpClient), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := (&Client{service: service, user: "me"}).Inventory(t.Context())
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
	service, err := gmail.NewService(t.Context(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{service: service, user: "me"}
	if err := client.Trash(t.Context(), "g1"); err != nil {
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
	service, err := gmail.NewService(t.Context(),
		option.WithEndpoint("https://gmail.test/"),
		option.WithHTTPClient(httpClient),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Client{service: service, user: "me"}).ChangesSince(t.Context(), 10)
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

func testGmailClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	service, err := gmail.NewService(t.Context(), option.WithEndpoint("https://gmail.test/"), option.WithHTTPClient(&http.Client{Transport: transport}), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return &Client{service: service, user: "me"}
}

func gmailResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestInventoryIncludesSpamTrashAndUnmarkedMessages(t *testing.T) {
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/gmail/v1/users/me/messages" {
			q := r.URL.Query()
			if q.Get("includeSpamTrash") != "true" || q.Get("q") != "" || q.Get("labelIds") != "" || q.Get("maxResults") != "500" {
				t.Fatalf("inventory parameters=%v", q)
			}
			return gmailResponse(r, 200, `{"messages":[{"id":"spam"},{"id":"trash"},{"id":"unmarked"},{"id":"gone"}]}`), nil
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/messages/spam":
			return gmailResponse(r, 200, `{"id":"spam","labelIds":["SPAM"],"payload":{}}`), nil
		case "/gmail/v1/users/me/messages/trash":
			return gmailResponse(r, 200, `{"id":"trash","labelIds":["TRASH"],"payload":{}}`), nil
		case "/gmail/v1/users/me/messages/unmarked":
			return gmailResponse(r, 200, `{"id":"unmarked","payload":{"headers":[{"name":"Message-ID","value":"<id@example.com>"}]}}`), nil
		case "/gmail/v1/users/me/messages/gone":
			return gmailResponse(r, 404, `{"error":{"code":404,"message":"gone"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", r.URL)
		}
	}))
	messages, err := client.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []syncer.Message{{ID: "spam", LabelIDs: []string{"SPAM"}}, {ID: "trash", LabelIDs: []string{"TRASH"}}, {ID: "unmarked", MessageIDHeaders: []string{"<id@example.com>"}}}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("inventory=%#v want %#v", messages, want)
	}
}

func TestInventoryAcceptsEmptyMailbox(t *testing.T) {
	for _, body := range []string{`{}`, `{"messages":[]}`} {
		t.Run(body, func(t *testing.T) {
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return gmailResponse(r, 200, body), nil }))
			if got, err := client.Inventory(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("inventory=%v error=%v", got, err)
			}
		})
	}
}

func TestInventoryRejectsMalformedAndIncompleteResponses(t *testing.T) {
	for _, tc := range []struct {
		name, list, metadata       string
		listStatus, metadataStatus int
		want                       string
	}{
		{name: "null list response", list: `null`, want: "null response"},
		{name: "null listed message", list: `{"messages":[null]}`, want: "null message"},
		{name: "missing listed ID", list: `{"messages":[{}]}`, want: "blank ID"},
		{name: "blank listed ID", list: `{"messages":[{"id":" "}]}`, want: "blank ID"},
		{name: "duplicate listed ID", list: `{"messages":[{"id":"g1"},{"id":"g1"}]}`, want: "duplicate ID"},
		{name: "null metadata", list: `{"messages":[{"id":"g1"}]}`, metadata: `null`, want: "does not match"},
		{name: "missing metadata ID", list: `{"messages":[{"id":"g1"}]}`, metadata: `{"payload":{}}`, want: "does not match"},
		{name: "mismatched metadata ID", list: `{"messages":[{"id":"g1"}]}`, metadata: `{"id":"other","payload":{}}`, want: "does not match"},
		{name: "missing payload", list: `{"messages":[{"id":"g1"}]}`, metadata: `{"id":"g1"}`, want: "missing payload"},
		{name: "list failure", listStatus: 404, list: `{"error":{"code":404,"message":"bad list"}}`, want: "list Gmail messages"},
		{name: "metadata forbidden", list: `{"messages":[{"id":"g1"}]}`, metadataStatus: 403, metadata: `{"error":{"code":403,"message":"forbidden"}}`, want: "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/gmail/v1/users/me/messages" {
					status := tc.listStatus
					if status == 0 {
						status = 200
					}
					return gmailResponse(r, status, tc.list), nil
				}
				body := tc.metadata
				if body == "" {
					body = `{"id":"g1","payload":{}}`
				}
				status := tc.metadataStatus
				if status == 0 {
					status = 200
				}
				return gmailResponse(r, status, body), nil
			}))
			if _, err := client.Inventory(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestInventoryRejectsPaginationCyclesAndDuplicateIDsAcrossPages(t *testing.T) {
	for _, tc := range []struct{ name, pages string }{
		{name: "cycle", pages: "cycle"},
		{name: "duplicate", pages: "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lists := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/gmail/v1/users/me/messages" {
					return gmailResponse(r, 200, `{"id":"g1","payload":{}}`), nil
				}
				lists++
				if lists > 3 {
					return nil, fmt.Errorf("unexpected fourth page")
				}
				if lists == 1 {
					return gmailResponse(r, 200, `{"messages":[{"id":"g1"}],"nextPageToken":"next"}`), nil
				}
				if tc.pages == "duplicate" {
					return gmailResponse(r, 200, `{"messages":[{"id":"g1"}]}`), nil
				}
				return gmailResponse(r, 200, `{"nextPageToken":"next"}`), nil
			}))
			_, err := client.Inventory(t.Context())
			want := "page token"
			if tc.pages == "duplicate" {
				want = "duplicate ID"
			}
			if err == nil || !strings.Contains(err.Error(), want) || lists != 2 {
				t.Fatalf("lists=%d error=%v want %q", lists, err, want)
			}
		})
	}
}

func TestChangesSinceIncludesMessageAddedOnEveryPage(t *testing.T) {
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !slices.Contains(r.URL.Query()["historyTypes"], "messageAdded") || !strings.Contains(r.URL.Query().Get("fields"), "messagesAdded(message(id))") {
			t.Fatalf("history parameters omit additions: %v", r.URL.Query())
		}
		if calls == 1 {
			return gmailResponse(r, 200, `{"history":[{"messagesAdded":[{"message":{"id":"new"}}]}],"nextPageToken":"next"}`), nil
		}
		if r.URL.Query().Get("pageToken") != "next" {
			t.Fatal("history did not paginate")
		}
		return gmailResponse(r, 200, `{"history":[{"labelsAdded":[{"message":{"id":"changed"}},{"message":{"id":"new"}}]}]}`), nil
	}))
	got, err := client.ChangesSince(t.Context(), 10)
	if err != nil || !reflect.DeepEqual(got, []string{"changed", "new"}) || calls != 2 {
		t.Fatalf("IDs=%v calls=%d error=%v", got, calls, err)
	}
}

func TestChangesSinceRejectsMalformedResponses(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"history":[null]}`,
		`{"history":[{"messagesAdded":[null]}]}`,
		`{"history":[{"messagesAdded":[{}]}]}`,
		`{"history":[{"messagesAdded":[{"message":{"id":""}}]}]}`,
		`{"history":[{"labelsAdded":[null]}]}`,
		`{"history":[{"labelsRemoved":[{}]}]}`,
		`{"history":[{"messagesDeleted":[{"message":{}}]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return gmailResponse(r, 200, body), nil }))
			if _, err := client.ChangesSince(t.Context(), 10); err == nil {
				t.Fatal("malformed history accepted")
			}
		})
	}
}

func TestChangesSinceRejectsPageTokenCycle(t *testing.T) {
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 3 {
			return nil, fmt.Errorf("unexpected fourth page")
		}
		return gmailResponse(r, 200, `{"nextPageToken":"next"}`), nil
	}))
	if _, err := client.ChangesSince(t.Context(), 10); err == nil || !strings.Contains(err.Error(), "page token") || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestChangesSinceExpiredHistorySentinel(t *testing.T) {
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return gmailResponse(r, 404, `{"error":{"code":404,"message":"expired history"}}`), nil
	}))
	if _, err := client.ChangesSince(t.Context(), 10); !errors.Is(err, syncer.ErrCannotCalculateDestinationChanges) {
		t.Fatalf("error=%v want expired history sentinel", err)
	}
}
