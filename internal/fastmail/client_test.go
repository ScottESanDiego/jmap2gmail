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

	"jmap2gmail/internal/syncer"
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
		case strings.Contains(body, `"jmap2gmail-state-probe"`):
			payload = `{"methodResponses":[["Email/get",{"accountId":"account-1","state":"e1","list":[],"notFound":["jmap2gmail-state-probe"]},"0"]],"sessionState":"session-1"}`
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
			payload = `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q1","position":1,"ids":["email-2"],"total":2},"0"]],"sessionState":"session-1"}`
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

func TestEmailIDsInMailboxesFiltersEveryPageAndRetry(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Calls [][]json.RawMessage `json:"methodCalls"`
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
		var args struct {
			Position int64 `json:"position"`
			Filter   struct {
				Operator   string `json:"operator"`
				Conditions []struct {
					InMailbox string `json:"inMailbox"`
				} `json:"conditions"`
			} `json:"filter"`
		}
		if len(body.Calls) != 1 {
			t.Fatalf("calls=%s", data)
		}
		if err := json.Unmarshal(body.Calls[0][1], &args); err != nil {
			return nil, err
		}
		if args.Filter.Operator != "OR" || len(args.Filter.Conditions) != 2 || args.Filter.Conditions[0].InMailbox != "inbox" || args.Filter.Conditions[1].InMailbox != "projects" {
			t.Fatalf("query lost filter: %s", data)
		}
		calls++
		var state string
		var ids []string
		total := 2
		switch calls {
		case 1:
			state = "q1"
			ids = []string{"old"}
			if args.Position != 0 {
				t.Fatal("bad first position")
			}
		case 2:
			state = "q2"
			ids = []string{"changed"}
			if args.Position != 1 {
				t.Fatal("bad second position")
			}
		case 3:
			state = "q3"
			ids = []string{"a"}
			if args.Position != 0 {
				t.Fatal("retry did not restart")
			}
		case 4:
			state = "q3"
			ids = []string{"b"}
			if args.Position != 1 {
				t.Fatal("partial page did not advance")
			}
		default:
			t.Fatalf("unexpected query %d", calls)
		}
		payload := fmt.Sprintf(`{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":%q,"position":%d,"ids":%s,"total":%d},"0"]],"sessionState":"session-1"}`, state, args.Position, mustJSON(t, ids), total)
		return response(200, "application/json", payload, r), nil
	})
	ids, err := testClient(transport).EmailIDsInMailboxes(t.Context(), []string{"inbox", "projects", "inbox"})
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) || calls != 4 {
		t.Fatalf("IDs=%v calls=%d error=%v", ids, calls, err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAllEmailIDsRemainsUnfiltered(t *testing.T) {
	client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(data), `"filter"`) {
			t.Fatalf("all query includes filter: %s", data)
		}
		return response(200, "application/json", `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q1","ids":[],"total":0},"0"]],"sessionState":"session-1"}`, r), nil
	}))
	if _, err := client.AllEmailIDs(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestEmailIDsInMailboxesEmptySelection(t *testing.T) {
	client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("empty selection queried server")
		return nil, nil
	}))
	if ids, err := client.EmailIDsInMailboxes(t.Context(), nil); err != nil || len(ids) != 0 {
		t.Fatalf("IDs=%v error=%v", ids, err)
	}
	if _, err := client.EmailIDsInMailboxes(t.Context(), []string{""}); err == nil {
		t.Fatal("empty mailbox ID accepted")
	}
}

func TestEmailsRequiresExactResponseAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		wantError    bool
		want         int
	}{
		{"found", `"list":[{"id":"a","blobId":"b"}]`, false, 1},
		{"not found", `"list":[],"notFound":["a"]`, false, 0},
		{"missing", `"list":[]`, true, 0},
		{"unknown", `"list":[{"id":"other"}]`, true, 0},
		{"duplicate", `"list":[{"id":"a"},{"id":"a"}]`, true, 0},
		{"both", `"list":[{"id":"a"}],"notFound":["a"]`, true, 0},
		{"unknown not found", `"list":[],"notFound":["other"]`, true, 0},
		{"duplicate not found", `"list":[],"notFound":["a","a"]`, true, 0},
		{"null email", `"list":[null]`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return response(200, "application/json", `{"methodResponses":[["Email/get",{"accountId":"account-1","state":"e1",`+tc.fields+`},"0"]],"sessionState":"session-1"}`, r), nil
			}))
			got, err := client.Emails(t.Context(), []string{"a"})
			if (err != nil) != tc.wantError || (err == nil && len(got) != tc.want) {
				t.Fatalf("emails=%v error=%v", got, err)
			}
		})
	}
}

func TestQueryRejectsIncompleteInventory(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"missing state", `"ids":[],"total":0`},
		{"missing IDs", `"queryState":"q","total":0`},
		{"empty before total", `"queryState":"q","ids":[],"total":1`},
		{"wrong position", `"queryState":"q","position":1,"ids":["a"],"total":1`},
		{"duplicates", `"queryState":"q","ids":["a","a"],"total":2`},
		{"empty ID", `"queryState":"q","ids":[""],"total":1`},
		{"missing total", `"queryState":"q","ids":["a"]`},
		{"too many", `"queryState":"q","ids":["a","b"],"total":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return response(200, "application/json", `{"methodResponses":[["Email/query",{"accountId":"account-1",`+tc.fields+`},"0"]],"sessionState":"session-1"}`, r), nil
			}))
			if _, err := client.EmailIDsInMailboxes(t.Context(), []string{"inbox"}); err == nil {
				t.Fatal("incomplete inventory accepted")
			}
		})
	}
}

func TestQueryRejectsTotalChangeWithoutStateChange(t *testing.T) {
	calls := 0
	client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		payload := `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q","ids":["a"],"total":2},"0"]],"sessionState":"session-1"}`
		if calls == 2 {
			payload = `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q","position":1,"ids":["b"],"total":3},"0"]],"sessionState":"session-1"}`
		}
		return response(200, "application/json", payload, r), nil
	}))
	if _, err := client.AllEmailIDs(t.Context()); err == nil {
		t.Fatal("inconsistent total accepted")
	}
}

func TestQueryRequiresExplicitTotal(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		wantError    bool
	}{
		{"missing empty", `"ids":[]`, true},
		{"null empty", `"ids":[],"total":null`, true},
		{"nonnumeric", `"ids":[],"total":"0"`, true},
		{"negative", `"ids":[],"total":-1`, true},
		{"explicit zero", `"ids":[],"total":0`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, scoped := range []bool{false, true} {
				client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return response(200, "application/json", `{"methodResponses":[["Email/query",{"accountId":"account-1","queryState":"q",`+tc.fields+`},"0"]],"sessionState":"session-1"}`, r), nil
				}))
				var ids []string
				var err error
				if scoped {
					ids, err = client.EmailIDsInMailboxes(t.Context(), []string{"inbox"})
				} else {
					ids, err = client.AllEmailIDs(t.Context())
				}
				if (err != nil) != tc.wantError || (err == nil && len(ids) != 0) {
					t.Fatalf("scoped=%v IDs=%v error=%v wantError=%v", scoped, ids, err, tc.wantError)
				}
			}
		})
	}
}

func TestQueryRequiresTotalOnLaterPages(t *testing.T) {
	for _, field := range []string{"", `,"total":null`} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"accountId":"account-1","queryState":"q","ids":["a"],"total":2}`
				if calls == 2 {
					body = `{"accountId":"account-1","queryState":"q","position":1,"ids":[]` + field + `}`
				}
				return response(200, "application/json", `{"methodResponses":[["Email/query",`+body+`,"0"]],"sessionState":"session-1"}`, r), nil
			}))
			if ids, err := client.AllEmailIDs(t.Context()); err == nil {
				t.Fatalf("later page without total accepted IDs=%v", ids)
			}
		})
	}
}
