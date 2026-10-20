package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func fakePolicy(t *testing.T) (*requestPolicy, *time.Time, *[]time.Duration) {
	t.Helper()
	p := newRequestPolicy(ClientOptions{})
	now := time.Unix(1700000000, 0)
	var waits []time.Duration
	p.now = func() time.Time { return now }
	p.jitter = func() time.Duration { return 0 }
	p.wait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, delay)
		now = now.Add(delay)
		return nil
	}
	return p, &now, &waits
}

func minuteQuota() error {
	return &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}
}

func TestQuotaClassification(t *testing.T) {
	errorInfo := func(reason string, metadata map[string]any) []any {
		return []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "metadata": metadata}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"legacy user", minuteQuota(), true},
		{"legacy project", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}, true},
		{"structured", &googleapi.Error{Code: 403, Details: errorInfo("RATE_LIMIT_EXCEEDED", map[string]any{"quota_limit": "QueriesPerMinutePerUser"})}, true},
		{"429", &googleapi.Error{Code: 429}, true},
		{"daily 403", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "dailyLimitExceeded"}}}, false},
		{"daily 429", &googleapi.Error{Code: 429, Details: errorInfo("RATE_LIMIT_EXCEEDED", map[string]any{"quota_limit": "QueriesPerDay"})}, false},
		{"quota budget", &googleapi.Error{Code: 429, Details: errorInfo("QUOTA_EXCEEDED", nil)}, false},
		{"permission", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}, false},
		{"auth", &googleapi.Error{Code: 401}, false},
		{"missing credentials", &googleapi.Error{Code: 429, Details: errorInfo("CREDENTIALS_MISSING", nil)}, false},
		{"invalid token", &googleapi.Error{Code: 429, Details: errorInfo("ACCESS_TOKEN_INVALID", nil)}, false},
		{"token type", &googleapi.Error{Code: 429, Details: errorInfo("ACCESS_TOKEN_TYPE_UNSUPPORTED", nil)}, false},
		{"account state", &googleapi.Error{Code: 429, Details: errorInfo("ACCOUNT_STATE_INVALID", nil)}, false},
		{"API key restriction", &googleapi.Error{Code: 429, Details: errorInfo("API_KEY_HTTP_REFERRER_BLOCKED", nil)}, false},
		{"legacy rate with daily detail", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}, Details: errorInfo("RATE_LIMIT_EXCEEDED", map[string]any{"quota_limit": "QueriesPerDayPerProject"})}, false},
		{"generic forbidden", &googleapi.Error{Code: 403}, false},
		{"network", io.ErrUnexpectedEOF, false},
		{"server", &googleapi.Error{Code: 503}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMinuteQuotaError(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("retry=%v want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaServerDelays(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name, header, retry string
		want                time.Duration
	}{
		{"seconds", "90", "", 90 * time.Second},
		{"date", now.Add(120 * time.Second).UTC().Format(http.TimeFormat), "", 120 * time.Second},
		{"past", now.Add(-time.Second).UTC().Format(http.TimeFormat), "", 0},
		{"detail", "", "2.500s", 2500 * time.Millisecond},
		{"latest wins", "90", "120s", 120 * time.Second},
		{"malformed", "garbage", "garbage", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{tc.header}}, Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": tc.retry}}}
			if got := serverRetryDelay(err, now); got != tc.want {
				t.Fatalf("delay=%v want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaRetriesBeyondFixedAttemptCaps(t *testing.T) {
	p, _, waits := fakePolicy(t)
	c := &Client{policy: p}
	var output bytes.Buffer
	p.output = &output
	calls := 0
	got, err := execute(t.Context(), c, "messages.insert", func(context.Context) (int, error) {
		calls++
		if calls <= 40 {
			return 0, minuteQuota()
		}
		return 42, nil
	})
	if err != nil || got != 42 || calls != 41 || len(*waits) != 40 {
		t.Fatalf("result=%d calls=%d waits=%d error=%v", got, calls, len(*waits), err)
	}
	for i, delay := range *waits {
		want := time.Second << min(i, 6)
		if delay != want {
			t.Fatalf("wait %d=%v want %v", i, delay, want)
		}
	}
	if !strings.Contains(output.String(), "rate limit; retrying messages.insert") {
		t.Fatalf("quota diagnostics=%q", output.String())
	}
}

func TestQuotaCancellationDuringBackoff(t *testing.T) {
	p, _, _ := fakePolicy(t)
	ctx, cancel := context.WithCancel(t.Context())
	p.wait = func(ctx context.Context, delay time.Duration) error { cancel(); return ctx.Err() }
	calls := 0
	_, err := execute(ctx, &Client{policy: p}, "labels.list", func(context.Context) (int, error) { calls++; return 0, minuteQuota() })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestQuotaRecoveryReportsRetriesAndElapsed(t *testing.T) {
	p, now, _ := fakePolicy(t)
	var output bytes.Buffer
	p.output = &output
	calls := 0
	result, err := execute(t.Context(), &Client{policy: p}, "messages.get", func(context.Context) (int, error) {
		calls++
		*now = now.Add(250 * time.Millisecond)
		if calls <= 2 {
			return 0, minuteQuota()
		}
		return 42, nil
	})
	want := "Gmail quota: rate limit; retrying messages.get, waiting 1s\n" +
		"Gmail quota: rate limit; retrying messages.get, waiting 2s\n" +
		"Gmail quota: recovered messages.get after 2 quota retries (elapsed 3.75s)\n"
	if err != nil || result != 42 || calls != 3 || output.String() != want {
		t.Fatalf("result=%d calls=%d error=%v diagnostics=%q, want %q", result, calls, err, output.String(), want)
	}
}

func TestQuotaFailuresNeverReportRecovery(t *testing.T) {
	for _, failure := range []string{"permanent", "request cancellation", "backoff failure", "backoff cancellation", "before retry cancellation"} {
		t.Run(failure, func(t *testing.T) {
			p, _, _ := fakePolicy(t)
			var output bytes.Buffer
			p.output = &output
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantErr := io.ErrUnexpectedEOF
			switch failure {
			case "request cancellation":
				wantErr = context.Canceled
			case "backoff failure":
				p.wait = func(context.Context, time.Duration) error { return wantErr }
			case "backoff cancellation":
				wantErr = context.Canceled
				p.wait = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
			case "before retry cancellation":
				wantErr = context.Canceled
				p.wait = func(context.Context, time.Duration) error { cancel(); return nil }
			}
			calls := 0
			_, err := execute(ctx, &Client{policy: p}, "messages.get", func(context.Context) (int, error) {
				calls++
				if calls == 1 {
					return 0, minuteQuota()
				}
				if failure == "request cancellation" {
					cancel()
				}
				return 0, wantErr
			})
			if !errors.Is(err, wantErr) || strings.Contains(output.String(), "recovered") {
				t.Fatalf("error=%v diagnostics=%q", err, output.String())
			}
		})
	}
}

func TestConcurrentQuotaRecoveryDiagnostics(t *testing.T) {
	var output bytes.Buffer
	p := newRequestPolicy(ClientOptions{Diagnostics: &output})
	p.jitter = func() time.Duration { return 0 }
	p.wait = func(context.Context, time.Duration) error { return nil }
	c := &Client{policy: p}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			calls := 0
			_, err := execute(t.Context(), c, "messages.get", func(context.Context) (int, error) {
				calls++
				if calls == 1 {
					return 0, minuteQuota()
				}
				return 1, nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 40 {
		t.Fatalf("diagnostic lines=%d, want 40: %q", len(lines), output.String())
	}
	for _, line := range lines {
		if line != "Gmail quota: rate limit; retrying messages.get, waiting 1s" &&
			!(strings.HasPrefix(line, "Gmail quota: recovered messages.get after 1 quota retries (elapsed ") && strings.HasSuffix(line, ")")) {
			t.Fatalf("interleaved or unexpected diagnostic: %q", line)
		}
	}
}

func TestQuotaBackoffOutsideAttemptTimeout(t *testing.T) {
	calls := 0
	client, err := New(t.Context(), &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return gmailResponse(r, 403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`), nil
		}
		return gmailResponse(r, 200, `{"labels":[]}`), nil
	})}, "me")
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := fakePolicy(t)
	p.attemptTimeout = 10 * time.Millisecond
	p.wait = func(ctx context.Context, delay time.Duration) error { return waitContext(ctx, 30*time.Millisecond) }
	client.policy = p
	started := time.Now()
	if _, err := client.Labels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || time.Since(started) < 30*time.Millisecond {
		t.Fatalf("calls=%d elapsed=%v", calls, time.Since(started))
	}
}

func TestSuccessfulRequestsNeverWait(t *testing.T) {
	p, _, waits := fakePolicy(t)
	var output bytes.Buffer
	p.output = &output
	client := &Client{policy: p}
	calls := 0
	for range 1000 {
		if _, err := execute(t.Context(), client, "messages.get", func(context.Context) (int, error) { calls++; return 1, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1000 || len(*waits) != 0 || output.Len() != 0 {
		t.Fatalf("calls=%d waits=%d diagnostics=%q", calls, len(*waits), output.String())
	}
}

func TestQuotaExecutorChecksCancellationBeforeEveryAttempt(t *testing.T) {
	for _, precanceled := range []bool{true, false} {
		t.Run(fmt.Sprint(precanceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p, _, _ := fakePolicy(t)
			p.wait = func(context.Context, time.Duration) error { cancel(); return nil }
			if precanceled {
				cancel()
			}
			calls := 0
			_, err := execute(ctx, &Client{policy: p}, "messages.get", func(context.Context) (int, error) { calls++; return 0, minuteQuota() })
			want := 1
			if precanceled {
				want = 0
			}
			if calls != want || !errors.Is(err, context.Canceled) {
				t.Fatalf("calls=%d want=%d error=%v", calls, want, err)
			}
		})
	}
}
