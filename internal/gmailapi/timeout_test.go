package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"jmap2gmail/internal/syncer"
)

func readTimeout() error {
	return fmt.Errorf("wrapped transport failure: %w", &net.DNSError{Err: "timeout", IsTimeout: true})
}

func TestReadTimeoutRetriesBeyondCapsWithFreshContexts(t *testing.T) {
	p, _, waits := fakePolicy(t)
	p.attemptTimeout = 15 * time.Millisecond
	var contexts []context.Context
	p.wait = func(ctx context.Context, delay time.Duration) error {
		if contexts[len(contexts)-1].Err() == nil {
			t.Fatal("attempt remained live during backoff")
		}
		*waits = append(*waits, delay)
		return ctx.Err()
	}
	calls := 0
	result, err := execute(t.Context(), &Client{policy: p}, "messages.get", func(ctx context.Context) (int, error) {
		for _, previous := range contexts {
			if ctx == previous {
				t.Fatal("reused attempt context")
			}
		}
		if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
			t.Fatal("new attempt lacks live deadline")
		}
		contexts = append(contexts, ctx)
		calls++
		if calls <= 40 {
			return 0, readTimeout()
		}
		return 42, nil
	})
	if err != nil || result != 42 || calls != 41 || len(*waits) != 40 {
		t.Fatalf("result=%d calls=%d waits=%d error=%v", result, calls, len(*waits), err)
	}
	for _, ctx := range contexts {
		if ctx.Err() == nil {
			t.Fatal("completed attempt context not canceled")
		}
	}
}

func TestTimeoutAllowlistAndPermanentFailures(t *testing.T) {
	for _, method := range []string{"labels.list", "getProfile", "messages.get", "messages.list", "history.list", "labels.create", "labels.patch", "messages.insert", "messages.modify", "messages.trash", "unknown"} {
		t.Run(method, func(t *testing.T) {
			p, _, waits := fakePolicy(t)
			calls := 0
			_, err := execute(t.Context(), &Client{policy: p}, method, func(context.Context) (int, error) {
				calls++
				if calls == 1 {
					return 0, context.DeadlineExceeded
				}
				return 1, nil
			})
			read := method == "labels.list" || method == "getProfile" || method == "messages.get" || method == "messages.list" || method == "history.list"
			if read && (err != nil || calls != 2 || len(*waits) != 1) || !read && (!errors.Is(err, context.DeadlineExceeded) || calls != 1 || len(*waits) != 0) {
				t.Fatalf("calls=%d waits=%d error=%v", calls, len(*waits), err)
			}
		})
	}
	for _, failure := range []error{io.EOF, io.ErrUnexpectedEOF, errors.New("network disconnected")} {
		p, _, waits := fakePolicy(t)
		calls := 0
		_, err := execute(t.Context(), &Client{policy: p}, "messages.get", func(context.Context) (int, error) { calls++; return 0, failure })
		if !errors.Is(err, failure) || calls != 1 || len(*waits) != 0 {
			t.Fatalf("permanent failure replayed: calls=%d waits=%d error=%v", calls, len(*waits), err)
		}
	}
}

func TestParentDeadlineAndCancellationNeverRetry(t *testing.T) {
	for _, where := range []string{"request", "backoff", "parent deadline"} {
		t.Run(where, func(t *testing.T) {
			p, _, waits := fakePolicy(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if where == "parent deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Millisecond)
				defer cancel()
			}
			calls := 0
			if where == "backoff" {
				p.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
			}
			_, err := execute(ctx, &Client{policy: p}, "messages.get", func(attemptCtx context.Context) (int, error) {
				calls++
				if where == "request" {
					cancel()
				}
				if where == "parent deadline" {
					<-attemptCtx.Done()
					return 0, attemptCtx.Err()
				}
				return 0, readTimeout()
			})
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			if calls != 1 || len(*waits) != 0 {
				t.Fatalf("parent termination retried: calls=%d waits=%d", calls, len(*waits))
			}
		})
	}
}

func TestMixedTimeoutAndQuotaRecoveryCounts(t *testing.T) {
	p, _, _ := fakePolicy(t)
	var output bytes.Buffer
	p.output = &output
	calls := 0
	_, err := execute(t.Context(), &Client{policy: p}, "messages.get", func(context.Context) (int, error) {
		calls++
		switch calls {
		case 1:
			return 0, readTimeout()
		case 2:
			return 0, minuteQuota()
		case 3:
			return 0, context.DeadlineExceeded
		}
		return 1, nil
	})
	if err != nil || !strings.Contains(output.String(), "recovered messages.get after 1 quota retries and 2 timeout retries (elapsed 7s)") {
		t.Fatalf("error=%v diagnostics=%q", err, output.String())
	}
}

func TestWriteTimeoutsNeverReplaySDKRequests(t *testing.T) {
	for _, method := range []string{"create", "rename", "insert", "modify", "trash"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, readTimeout() }))
			p, _, waits := fakePolicy(t)
			client.policy = p
			var err error
			switch method {
			case "create":
				_, err = client.CreateLabel(t.Context(), "folder")
			case "rename":
				_, err = client.RenameLabel(t.Context(), "label", "folder")
			case "insert":
				_, err = client.Insert(t.Context(), strings.NewReader("Subject: test\r\n\r\nbody"), nil)
			case "modify":
				_, err = client.ModifyLabels(t.Context(), "message", nil, nil)
			case "trash":
				err = client.Trash(t.Context(), "message")
			}
			if err == nil || !isTimeoutError(err) || calls != 1 || len(*waits) != 0 {
				t.Fatalf("calls=%d waits=%d error=%v", calls, len(*waits), err)
			}
		})
	}
}

// A mutex also permits a useful diagnostic if an unrelated server logger writes
// while the timeout fixture inspects captured standard-library output.
type timeoutLog struct {
	mu     sync.Mutex
	output bytes.Buffer
}

func (l *timeoutLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.output.Write(p)
}
func (l *timeoutLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.output.String() }

func TestOAuthSDKAttemptTimeoutIncludesDecodingWithoutLegacyCancellation(t *testing.T) {
	for _, stall := range []string{"headers", "decoding"} {
		t.Run(stall, func(t *testing.T) {
			credentials, path := oauthFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected token refresh") }, &oauth2.Token{AccessToken: "valid", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
			transportClient, err := AuthenticatedClient(t.Context(), credentials, path, false)
			if err != nil {
				t.Fatal(err)
			}
			if transportClient.Timeout != 0 {
				t.Fatal("OAuth client still owns a request timer")
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if stall == "decoding" {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"labels":[`)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				io.WriteString(w, `{"labels":[]}`)
			}))
			defer server.Close()
			var contexts []context.Context
			oauthTransport := transportClient.Transport.(*oauth2.Transport)
			oauthTransport.Base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Cancel != nil {
					t.Error("deprecated request.Cancel populated")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("OAuth transport request lacks attempt deadline")
				}
				contexts = append(contexts, r.Context())
				if r.Header.Get("Authorization") != "Bearer valid" {
					t.Error("request lacks OAuth bearer")
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			client, err := New(t.Context(), transportClient, "me")
			if err != nil {
				t.Fatal(err)
			}
			client.service.BasePath = server.URL + "/"
			p, _, _ := fakePolicy(t)
			p.attemptTimeout = 40 * time.Millisecond
			client.policy = p
			var captured timeoutLog
			previous := log.Writer()
			log.SetOutput(&captured)
			defer log.SetOutput(previous)
			if _, err := client.Labels(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || len(contexts) != 2 || !errors.Is(contexts[0].Err(), context.DeadlineExceeded) || !errors.Is(contexts[1].Err(), context.Canceled) {
				t.Fatalf("calls=%d contexts=%v", calls.Load(), contexts)
			}
			if strings.Contains(captured.String(), "CancelRequest") || strings.Contains(captured.String(), "deprecated") {
				t.Fatalf("legacy cancellation log: %s", captured.String())
			}
		})
	}
}

func TestNewPreservesCallerHTTPClientTimeout(t *testing.T) {
	transportClient := &http.Client{Timeout: 7 * time.Second}
	client, err := New(t.Context(), transportClient, "me")
	if err != nil {
		t.Fatal(err)
	}
	if transportClient.Timeout != 7*time.Second || client.policy.attemptTimeout != 60*time.Second {
		t.Fatalf("client timeout=%v attempt timeout=%v", transportClient.Timeout, client.policy.attemptTimeout)
	}
}

func TestConclusiveSDKResultsSurviveDeadlineRaces(t *testing.T) {
	for _, kind := range []string{"child deadline success", "parent cancellation success", "HTTP 400"} {
		t.Run(kind, func(t *testing.T) {
			p, _, waits := fakePolicy(t)
			p.attemptTimeout = time.Millisecond
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			result, err := execute(parent, &Client{policy: p}, "messages.get", func(ctx context.Context) (int, error) {
				calls++
				if kind == "parent cancellation success" {
					cancel()
				} else {
					<-ctx.Done()
				}
				if kind == "HTTP 400" {
					return 0, &googleapi.Error{Code: 400}
				}
				return 42, nil
			})
			if calls != 1 || len(*waits) != 0 {
				t.Fatalf("conclusive outcome retried: calls=%d waits=%d", calls, len(*waits))
			}
			if kind == "HTTP 400" {
				if !isHTTPStatus(err, 400) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("conclusive rejection changed: %v", err)
				}
			} else if err != nil || result != 42 {
				t.Fatalf("confirmed result lost: result=%d error=%v", result, err)
			}
		})
	}
}

func TestInsertBadRequestAfterAttemptDeadlineRemainsDefinite(t *testing.T) {
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Body != nil {
			defer r.Body.Close()
		}
		<-r.Context().Done()
		return gmailResponse(r, 400, `{"error":{"code":400,"message":"rejected"}}`), nil
	}))
	p, _, waits := fakePolicy(t)
	p.attemptTimeout = time.Millisecond
	client.policy = p
	_, err := client.Insert(t.Context(), strings.NewReader("Subject: test\r\n\r\nbody"), nil)
	if calls != 1 || len(*waits) != 0 || !errors.Is(err, syncer.ErrMutationRejected) || !isHTTPStatus(err, 400) {
		t.Fatalf("calls=%d waits=%d error=%v", calls, len(*waits), err)
	}
}

func TestInsertConfirmedBadRequestPreservesConcurrentParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Body != nil {
			defer r.Body.Close()
		}
		cancel()
		return gmailResponse(r, 400, `{"error":{"code":400,"message":"rejected"}}`), nil
	}))
	p, _, waits := fakePolicy(t)
	client.policy = p
	_, err := client.Insert(parent, strings.NewReader("Subject: test\r\n\r\nbody"), nil)
	if calls != 1 || len(*waits) != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, syncer.ErrMutationRejected) || !isHTTPStatus(err, 400) {
		t.Fatalf("calls=%d waits=%d error=%v", calls, len(*waits), err)
	}
}

func TestTimeoutRetriesOnlyFailedInventoryPage(t *testing.T) {
	var tokens, gets []string
	failed := false
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/gmail/v1/users/me/messages" {
			token := r.URL.Query().Get("pageToken")
			tokens = append(tokens, token)
			if token == "" {
				return gmailResponse(r, 200, `{"messages":[{"id":"g1"}],"nextPageToken":"next"}`), nil
			}
			if !failed {
				failed = true
				return nil, readTimeout()
			}
			return gmailResponse(r, 200, `{"messages":[{"id":"g2"}]}`), nil
		}
		id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
		gets = append(gets, id)
		return gmailResponse(r, 200, fmt.Sprintf(`{"id":%q,"payload":{}}`, id)), nil
	}))
	p, _, waits := fakePolicy(t)
	client.policy = p
	messages, err := client.Inventory(t.Context())
	if err != nil || len(messages) != 2 || !reflect.DeepEqual(tokens, []string{"", "next", "next"}) || !reflect.DeepEqual(gets, []string{"g1", "g2"}) || len(*waits) != 1 {
		t.Fatalf("messages=%v pages=%v gets=%v waits=%v error=%v", messages, tokens, gets, *waits, err)
	}
}
