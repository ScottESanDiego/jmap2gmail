package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
)

func fakePolicy(t *testing.T) (*requestPolicy, *time.Time, *[]time.Duration) {
	t.Helper()
	p, err := newRequestPolicy(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := execute(t.Context(), c, "messages.insert", func() (int, error) {
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
	if !strings.Contains(output.String(), "rate limit; retrying messages.insert (25 units)") {
		t.Fatalf("quota diagnostics=%q", output.String())
	}
}

func TestQuotaChargesEveryAttemptAndMethod(t *testing.T) {
	for method, cost := range map[string]int{"labels.list": 1, "labels.create": 5, "labels.patch": 5, "getProfile": 1, "history.list": 2, "messages.list": 5, "messages.get": 20, "messages.insert": 25, "messages.modify": 5, "messages.trash": 20} {
		t.Run(method, func(t *testing.T) {
			p, now, _ := fakePolicy(t)
			p.wait = func(ctx context.Context, delay time.Duration) error { return ctx.Err() }
			calls := 0
			_, err := execute(t.Context(), &Client{policy: p}, method, func() (int, error) {
				calls++
				if calls == 1 {
					return 0, minuteQuota()
				}
				return 1, nil
			})
			if err != nil || calls != 2 || p.limiter.TokensAt(*now) != float64(100-2*cost) {
				t.Fatalf("calls=%d tokens=%v error=%v", calls, p.limiter.TokensAt(*now), err)
			}
		})
	}
}

func TestQuotaCancellationRestoresReservation(t *testing.T) {
	p, now, _ := fakePolicy(t)
	p.limiter = rate.NewLimiter(80, 100)
	if !p.limiter.AllowN(*now, 100) {
		t.Fatal("cannot exhaust initial burst")
	}
	ctx, cancel := context.WithCancel(t.Context())
	p.wait = func(ctx context.Context, delay time.Duration) error { cancel(); return ctx.Err() }
	calls := 0
	_, err := execute(ctx, &Client{policy: p}, "messages.get", func() (int, error) { calls++; return 0, nil })
	if !errors.Is(err, context.Canceled) || calls != 0 || p.limiter.TokensAt(*now) != 0 {
		t.Fatalf("calls=%d tokens=%v error=%v", calls, p.limiter.TokensAt(*now), err)
	}
	p.wait = func(ctx context.Context, delay time.Duration) error { *now = now.Add(delay); return ctx.Err() }
	if err := p.acquire(t.Context(), "messages.get", 20); err != nil || p.limiter.TokensAt(*now) != 0 {
		t.Fatalf("reservation not reusable: tokens=%v error=%v", p.limiter.TokensAt(*now), err)
	}
}

func TestQuotaCancellationDuringBackoff(t *testing.T) {
	p, _, _ := fakePolicy(t)
	ctx, cancel := context.WithCancel(t.Context())
	p.wait = func(ctx context.Context, delay time.Duration) error { cancel(); return ctx.Err() }
	calls := 0
	_, err := execute(ctx, &Client{policy: p}, "labels.list", func() (int, error) { calls++; return 0, minuteQuota() })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestQuotaPacingAndLogThrottling(t *testing.T) {
	p, now, waits := fakePolicy(t)
	var output bytes.Buffer
	p.output = &output
	c := &Client{policy: p}
	for range 100 {
		if _, err := execute(t.Context(), c, "messages.get", func() (int, error) { return 0, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := now.Sub(time.Unix(1700000000, 0)); elapsed != 23750*time.Millisecond || len(*waits) != 95 {
		t.Fatalf("elapsed=%v waits=%d", elapsed, len(*waits))
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "messages.get (20 units)") {
		t.Fatalf("pacing logs=%q", output.String())
	}
}

func TestQuotaBackoffOutsideHTTPTimeout(t *testing.T) {
	calls := 0
	client, err := New(t.Context(), &http.Client{Timeout: 10 * time.Millisecond, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
