package fastmail

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"

	"jmapsync/internal/syncer"
)

func TestClientMapsJMAPResponses(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			if got, want := request.URL.Path, "/download/account-1/blob-1/filename"; got != want {
				return nil, fmt.Errorf("download path = %q, want %q", got, want)
			}
			return response(http.StatusOK, "message/rfc822", "Subject: test\r\n\r\nbody", request), nil
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body := string(data)
		var payload string
		switch {
		case strings.Contains(body, `"Mailbox/get"`):
			payload = `{"methodResponses":[["Mailbox/get",{"accountId":"account-1","state":"m1","list":[{"id":"inbox","name":"Inbox","role":"inbox"}]},"0"]],"sessionState":"session-1"}`
		case strings.Contains(body, `"Email/query"`):
			payload = `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q1","ids":["email-1"],"total":1},"0"]],"sessionState":"session-1"}`
		case strings.Contains(body, `"Email/changes"`):
			payload = `{"methodResponses":[["Email/changes",{"accountId":"account-1","oldState":"e1","newState":"e2","created":["email-2"],"updated":["email-1"],"destroyed":["email-0"]},"0"]],"sessionState":"session-1"}`
		case strings.Contains(body, `"jmapsync-state-probe"`):
			payload = `{"methodResponses":[["Email/get",{"accountId":"account-1","state":"e1","list":[],"notFound":["jmapsync-state-probe"]},"0"]],"sessionState":"session-1"}`
		case strings.Contains(body, `"Email/get"`):
			payload = `{"methodResponses":[["Email/get",{"accountId":"account-1","state":"e1","list":[{"id":"email-1","blobId":"blob-1","mailboxIds":{"inbox":true},"keywords":{"$seen":true},"messageId":["m1@example.com"],"size":42}]},"0"]],"sessionState":"session-1"}`
		default:
			return nil, fmt.Errorf("unexpected JMAP request: %s", body)
		}
		return response(http.StatusOK, "application/json", payload, request), nil
	})
	client := testClient(transport)
	ctx := t.Context()

	mailboxes, err := client.Mailboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []syncer.Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}; !reflect.DeepEqual(mailboxes, want) {
		t.Fatalf("mailboxes = %#v, want %#v", mailboxes, want)
	}
	if state, err := client.State(ctx); err != nil || state != "e1" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if ids, err := client.AllEmailIDs(ctx); err != nil || !reflect.DeepEqual(ids, []string{"email-1"}) {
		t.Fatalf("IDs = %v, error = %v", ids, err)
	}
	emails, err := client.Emails(ctx, []string{"email-1"})
	if err != nil {
		t.Fatal(err)
	}
	wantEmails := []syncer.Email{{
		ID: "email-1", BlobID: "blob-1", MailboxIDs: map[string]bool{"inbox": true},
		Keywords: map[string]bool{"$seen": true}, MessageIDs: []string{"m1@example.com"}, Size: 42,
	}}
	if !reflect.DeepEqual(emails, wantEmails) {
		t.Fatalf("emails = %#v, want %#v", emails, wantEmails)
	}
	changes, err := client.Changes(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	wantChanges := syncer.Changes{
		NewState: "e2", Created: []string{"email-2"}, Updated: []string{"email-1"}, Destroyed: []string{"email-0"},
	}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("changes = %#v, want %#v", changes, wantChanges)
	}
	raw, err := client.Raw(ctx, "blob-1")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if data, err := io.ReadAll(raw); err != nil || string(data) != "Subject: test\r\n\r\nbody" {
		t.Fatalf("raw = %q, error = %v", data, err)
	}
}

func TestAllEmailIDsAdvancesByReturnedPageSize(t *testing.T) {
	page := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		page++
		body := string(data)
		var payload string
		switch page {
		case 1:
			if strings.Contains(body, `"position"`) {
				return nil, fmt.Errorf("first page set a position: %s", body)
			}
			payload = `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q1","ids":["email-1"],"total":2},"0"]],"sessionState":"session-1"}`
		case 2:
			if !strings.Contains(body, `"position":1`) {
				return nil, fmt.Errorf("second page did not use returned count: %s", body)
			}
			payload = `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q1","ids":["email-2"],"total":2},"0"]],"sessionState":"session-1"}`
		default:
			return nil, fmt.Errorf("unexpected page %d", page)
		}
		return response(http.StatusOK, "application/json", payload, request), nil
	})
	ids, err := testClient(transport).AllEmailIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"email-1", "email-2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("IDs = %v, want %v", ids, want)
	}
}

func testClient(transport http.RoundTripper) *Client {
	raw := &jmap.Client{
		HttpClient:      &http.Client{Transport: transport},
		SessionEndpoint: "https://fastmail.test/session",
		Session: &jmap.Session{
			Username:    "user@example.com",
			APIURL:      "https://fastmail.test/api",
			DownloadURL: "https://fastmail.test/download/{accountId}/{blobId}/{name}?type={type}",
			RawCapabilities: map[jmap.URI]json.RawMessage{
				jmap.CoreURI: json.RawMessage(`{}`),
				mail.URI:     json.RawMessage(`{}`),
			},
		},
	}
	return &Client{client: raw, account: "account-1"}
}

func response(status int, contentType, body string, request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
