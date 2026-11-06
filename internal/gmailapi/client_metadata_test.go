package gmailapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"jmap2gmail/internal/store"
	"jmap2gmail/internal/syncer"
)

func TestMetadataMissingPayloadFallback(t *testing.T) {
	for _, first := range []string{`{"id":"g1"}`, `{"id":"g1","payload":null}`} {
		t.Run(first, func(t *testing.T) {
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/gmail/v1/users/me/messages/g1" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 60*time.Second {
					t.Fatalf("metadata attempt deadline = %v, present=%t", deadline, ok)
				}
				wantQuery := url.Values{"alt": {"json"}, "format": {"metadata"}, "prettyPrint": {"false"}}
				if calls == 1 {
					wantQuery["fields"] = []string{"id,threadId,labelIds,payload(headers)"}
					wantQuery["metadataHeaders"] = []string{syncer.HeaderAccount, syncer.HeaderEmailID, "Message-ID"}
				} else {
					wantQuery["fields"] = []string{"id,threadId,labelIds,payload"}
				}
				if !reflect.DeepEqual(r.URL.Query(), wantQuery) {
					t.Fatalf("call %d query=%v want %v", calls, r.URL.Query(), wantQuery)
				}
				if calls == 1 {
					return gmailResponse(r, 200, first), nil
				}
				if calls > 2 {
					t.Fatal("repeated payload fallback")
				}
				return gmailResponse(r, 200, `{"id":"g1","labelIds":["INBOX"],"payload":{"headers":[null,{"name":"X-Jmap2gmail-Account","value":"account"},{"name":"x-jmap2gmail-account","value":"account2"},{"name":"X-Jmap2gmail-Email-ID","value":"e1"},{"name":"x-jmap2gmail-email-id","value":""},{"name":"Message-ID","value":"<m1>"},{"name":"message-id","value":"<m2>"},{"name":"Subject","value":"private subject"},{"name":"From","value":"private sender"}]}}`), nil
			}))
			got, err := client.metadataMessage(t.Context(), "g1")
			want := syncer.Message{ID: "g1", LabelIDs: []string{"INBOX"}, AccountHeaders: []string{"account", "account2"}, EmailIDHeaders: []string{"e1", ""}, MessageIDHeaders: []string{"<m1>", "<m2>"}}
			if err != nil || calls != 2 || !reflect.DeepEqual(got, want) {
				t.Fatalf("message=%#v calls=%d error=%v", got, calls, err)
			}
		})
	}
}

func TestMetadataUnresolvedPayloadActionableError(t *testing.T) {
	for _, tc := range []struct{ name, firstThread, fallbackThread, user string }{
		{"fallback thread", "first", "thread/with?#\n", "configured@example.com"},
		{"first thread", "first", "", "me"},
		{"no thread", "", "", "me"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				thread := tc.firstThread
				if calls == 2 {
					thread = tc.fallbackThread
				}
				if calls > 2 {
					t.Fatal("unbounded fallback")
				}
				return gmailResponse(r, 200, fmt.Sprintf(`{"id":"g1","threadId":%q}`, thread)), nil
			}))
			client.user = tc.user
			_, err := client.metadataMessage(t.Context(), "g1")
			if err == nil || calls != 2 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if strings.Contains(err.Error(), "me means the authenticated account") != (tc.user == "me") {
				t.Fatalf("unexpected authenticated-account explanation: %v", err)
			}
			for _, want := range []string{`id="g1"`, fmt.Sprintf("userId=%q", tc.user), "Retry once", "correct authorized account", "More (three dots) > Show original", "https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/get", "format=metadata", "cannot use incomplete identity metadata"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			thread := tc.fallbackThread
			if thread == "" {
				thread = tc.firstThread
			}
			if thread != "" {
				for _, want := range []string{fmt.Sprintf("threadId=%q", thread), "https://mail.google.com/mail/u/0/#all/" + url.PathEscape(thread), "Best-effort", "select the correct account", "not guaranteed"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing %q", err, want)
					}
				}
			} else if strings.Contains(err.Error(), "mail.google.com") || strings.Contains(err.Error(), "threadId=") {
				t.Fatalf("invented thread locator: %v", err)
			}
		})
	}
}

func TestMetadataIDsFailClosed(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"id":""}`, `{"id":" "}`, `{"id":"other"}`} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback=%t", body, fallback), func(t *testing.T) {
				calls := 0
				client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if fallback && calls == 1 {
						return gmailResponse(r, 200, `{"id":"g1"}`), nil
					}
					return gmailResponse(r, 200, body), nil
				}))
				_, err := client.metadataMessage(t.Context(), "g1")
				wantCalls := 1
				if fallback {
					wantCalls++
				}
				if err == nil || !strings.Contains(err.Error(), "does not match") || calls != wantCalls {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			})
		}
	}
}

func TestMetadataPresentEmptyPayloadDoesNotFallback(t *testing.T) {
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return gmailResponse(r, 200, `{"id":"g1","payload":{}}`), nil
	}))
	if got, err := client.metadataMessage(t.Context(), "g1"); err != nil || got.ID != "g1" || calls != 1 {
		t.Fatalf("message=%v calls=%d error=%v", got, calls, err)
	}
}

func TestMetadataAPIErrorsPreserveCause(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, status := range []int{400, 401, 404} {
			t.Run(fmt.Sprintf("%d/fallback=%t", status, fallback), func(t *testing.T) {
				calls := 0
				client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if fallback && calls == 1 {
						return gmailResponse(r, 200, `{"id":"g1"}`), nil
					}
					return gmailResponse(r, status, fmt.Sprintf(`{"error":{"code":%d,"message":"API rejection"}}`, status)), nil
				}))
				_, err := client.metadataMessage(t.Context(), "g1")
				wantCalls := 1
				if fallback {
					wantCalls++
				}
				if calls != wantCalls || err == nil {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				if status == 404 {
					if !errors.Is(err, syncer.ErrNotFound) {
						t.Fatalf("404 error=%v", err)
					}
				} else if apiErr, ok := errors.AsType[*googleapi.Error](err); !ok || apiErr.Code != status {
					t.Fatalf("API cause lost: %v", err)
				}
			})
		}
	}
}

func TestInventorySkipsFallbackNotFound(t *testing.T) {
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return gmailResponse(r, 200, `{"messages":[{"id":"g1"}]}`), nil
		case 2:
			return gmailResponse(r, 200, `{"id":"g1"}`), nil
		default:
			return gmailResponse(r, 404, `{"error":{"code":404}}`), nil
		}
	}))
	if got, err := client.Inventory(t.Context()); err != nil || len(got) != 0 || calls != 3 {
		t.Fatalf("messages=%v calls=%d error=%v", got, calls, err)
	}
}

func TestMetadataFallbackUsesRequestPolicy(t *testing.T) {
	for _, failure := range []string{"quota", "timeout", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return gmailResponse(r, 200, `{"id":"g1"}`), nil
				}
				if r.URL.Query().Has("metadataHeaders") || r.URL.Query().Get("fields") != "id,threadId,labelIds,payload" {
					t.Fatalf("fallback retry changed query: %v", r.URL.Query())
				}
				if calls == 2 {
					switch failure {
					case "quota":
						return gmailResponse(r, 429, `{"error":{"code":429}}`), nil
					case "timeout":
						return nil, readTimeout()
					default:
						cancel()
						return nil, ctx.Err()
					}
				}
				return gmailResponse(r, 200, `{"id":"g1","payload":{}}`), nil
			}))
			policy, _, waits := fakePolicy(t)
			client.policy = policy
			_, err := client.metadataMessage(ctx, "g1")
			if failure == "cancellation" {
				if !errors.Is(err, context.Canceled) || calls != 2 || len(*waits) != 0 {
					t.Fatalf("calls=%d waits=%v error=%v", calls, *waits, err)
				}
			} else if err != nil || calls != 3 || len(*waits) != 1 {
				t.Fatalf("calls=%d waits=%v error=%v", calls, *waits, err)
			}
		})
	}
}

// Embed the unused source methods: a failed full inventory must stop before
// any reconciliation, raw-message read, or source change polling.
type metadataPreflightSource struct{ syncer.Source }

func (metadataPreflightSource) SourceIdentity(context.Context) (string, error) { return "fixture", nil }
func (metadataPreflightSource) Mailboxes(context.Context) ([]syncer.Mailbox, error) {
	return []syncer.Mailbox{{ID: "custom", Name: "Create Me"}}, nil
}
func (metadataPreflightSource) State(context.Context) (string, error)         { return "s1", nil }
func (metadataPreflightSource) AllEmailIDs(context.Context) ([]string, error) { return nil, nil }

type metadataPreflightStore struct{ saves int }

func (*metadataPreflightStore) Load() (*store.State, error) { return store.New(), nil }
func (s *metadataPreflightStore) Save(*store.State) error   { s.saves++; return nil }

func TestMetadataFallbackFailurePreventsFullPreflightMutations(t *testing.T) {
	account := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
	for _, payload := range []string{
		`null`,
		`{"headers":[{"name":"X-Jmap2gmail-Account","value":"wrong"},{"name":"X-Jmap2gmail-Email-ID","value":"e1"}]}`,
		fmt.Sprintf(`{"headers":[{"name":"X-Jmap2gmail-Account","value":%q},{"name":"X-Jmap2gmail-Account","value":%q},{"name":"X-Jmap2gmail-Email-ID","value":"e1"}]}`, account, account),
		fmt.Sprintf(`{"headers":[{"name":"X-Jmap2gmail-Account","value":%q},{"name":"X-Jmap2gmail-Email-ID","value":""}]}`, account),
	} {
		t.Run(payload, func(t *testing.T) {
			gets, mutations := 0, 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					mutations++
					return nil, fmt.Errorf("unexpected mutation %s", r.URL)
				}
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					return gmailResponse(r, 200, `{"emailAddress":"fixture@example.com","historyId":"1"}`), nil
				case "/gmail/v1/users/me/labels":
					return gmailResponse(r, 200, fmt.Sprintf(`{"labels":[{"id":"marker","name":"jmap2gmail-managed/%x","type":"user"}]}`, sha256.Sum256([]byte("fixture")))), nil
				case "/gmail/v1/users/me/messages":
					return gmailResponse(r, 200, `{"messages":[{"id":"g1"}]}`), nil
				case "/gmail/v1/users/me/messages/g1":
					gets++
					if gets == 1 {
						return gmailResponse(r, 200, `{"id":"g1"}`), nil
					}
					return gmailResponse(r, 200, `{"id":"g1","labelIds":["marker"],"payload":`+payload+`}`), nil
				default:
					return nil, fmt.Errorf("unexpected request %s", r.URL)
				}
			}))
			state := &metadataPreflightStore{}
			_, err := (&syncer.Engine{Source: metadataPreflightSource{}, Destination: client, Store: state, Options: syncer.Options{Apply: true, Delete: true, Full: true}}).Run(t.Context())
			want := "invalid identity headers"
			if payload == "null" {
				want = "missing payload after one fallback"
			}
			if err == nil || !strings.Contains(err.Error(), want) || gets != 2 || mutations != 0 || state.saves != 0 {
				t.Fatalf("gets=%d mutations=%d saves=%d error=%v", gets, mutations, state.saves, err)
			}
		})
	}
}
