package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
)

const DefaultQuotaUnitsPerMinute = 4800

// ClientOptions controls the shared quota budget for this Gmail client.
type ClientOptions struct {
	QuotaUnitsPerMinute int
	Diagnostics         io.Writer
}

type requestPolicy struct {
	limiter *rate.Limiter
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
	jitter  func() time.Duration
	output  io.Writer
	logMu   sync.Mutex
	lastLog time.Time
	logged  bool
}

func newRequestPolicy(options ClientOptions) (*requestPolicy, error) {
	units := options.QuotaUnitsPerMinute
	if units < 0 {
		return nil, fmt.Errorf("Gmail quota units per minute must not be negative")
	}
	if units == 0 {
		units = DefaultQuotaUnitsPerMinute
	}
	output := options.Diagnostics
	if output == nil {
		output = io.Discard
	}
	return &requestPolicy{
		limiter: rate.NewLimiter(rate.Limit(float64(units)/60), 100),
		now:     time.Now, wait: waitContext,
		jitter: func() time.Duration { return rand.N(time.Second) }, output: output,
	}, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Costs follow Gmail's current quota table. Both metadata and minimal gets
// cost 20; labels.patch is budgeted as a label update (5 units).
// Verified 2026-10-18: https://developers.google.com/workspace/gmail/api/reference/quota
func methodCost(method string) (int, error) {
	switch method {
	case "labels.list", "getProfile":
		return 1, nil
	case "history.list":
		return 2, nil
	case "labels.create", "labels.patch", "messages.list", "messages.modify":
		return 5, nil
	case "messages.get", "messages.trash":
		return 20, nil
	case "messages.insert":
		return 25, nil
	default:
		return 0, fmt.Errorf("unknown Gmail quota method %q", method)
	}
}

func (p *requestPolicy) logWait(method string, cost int, delay time.Duration, retry bool) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	now := p.now()
	if !retry && p.logged && now.Sub(p.lastLog) < 5*time.Second {
		return
	}
	if !retry {
		p.lastLog, p.logged = now, true
	}
	reason := "pacing"
	if retry {
		reason = "rate limit; retrying"
	}
	fmt.Fprintf(p.output, "Gmail quota: %s %s (%d units), waiting %s\n", reason, method, cost, delay.Round(time.Millisecond))
}

func (p *requestPolicy) acquire(ctx context.Context, method string, cost int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reservation := p.limiter.ReserveN(p.now(), cost)
	if !reservation.OK() {
		return fmt.Errorf("Gmail quota cost %d exceeds limiter burst", cost)
	}
	delay := reservation.DelayFrom(p.now())
	if delay > 0 {
		p.logWait(method, cost, delay, false)
		if err := p.wait(ctx, delay); err != nil {
			reservation.CancelAt(p.now())
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		reservation.CancelAt(p.now())
		return err
	}
	return nil
}

// execute surrounds a single fresh SDK call. Waiting happens before Do, outside
// the HTTP client's per-attempt timeout. Only explicit quota rejections replay.
func execute[T any](ctx context.Context, c *Client, method string, attempt func() (T, error)) (T, error) {
	var zero T
	cost, err := methodCost(method)
	if err != nil {
		return zero, err
	}
	c.policyOnce.Do(func() {
		if c.policy == nil {
			c.policy, _ = newRequestPolicy(ClientOptions{})
		}
	})
	p := c.policy
	backoff := time.Second
	for {
		if err := p.acquire(ctx, method, cost); err != nil {
			return zero, fmt.Errorf("wait for Gmail quota: %w", err)
		}
		result, err := attempt()
		if !isMinuteQuotaError(err) {
			return result, err
		}
		delay := min(backoff+p.jitter(), 64*time.Second)
		delay = max(delay, serverRetryDelay(err, p.now()))
		p.logWait(method, cost, delay, true)
		if err := p.wait(ctx, delay); err != nil {
			return zero, fmt.Errorf("wait to retry Gmail quota: %w", err)
		}
		backoff = min(backoff*2, 64*time.Second)
	}
}

func isMinuteQuotaError(err error) bool {
	apiErr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || (apiErr.Code != http.StatusForbidden && apiErr.Code != http.StatusTooManyRequests) {
		return false
	}
	rateLimited := apiErr.Code == http.StatusTooManyRequests
	for _, item := range apiErr.Errors {
		switch item.Reason {
		case "dailyLimitExceeded", "dailyLimitExceededUnreg", "quotaExceeded", "authError", "insufficientPermissions", "accessNotConfigured", "billingNotActive":
			return false
		case "rateLimitExceeded", "userRateLimitExceeded":
			rateLimited = true
		}
	}
	for _, detail := range apiErr.Details {
		value, ok := detail.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := value["@type"].(string); !strings.HasSuffix(kind, "/google.rpc.ErrorInfo") {
			continue
		}
		reason, _ := value["reason"].(string)
		if strings.HasPrefix(reason, "API_KEY_") || strings.HasPrefix(reason, "ACCESS_TOKEN_") || strings.HasPrefix(reason, "CREDENTIALS_") || strings.HasPrefix(reason, "AUTHENTICATION_") {
			return false
		}
		switch reason {
		case "QUOTA_EXCEEDED", "DAILY_LIMIT_EXCEEDED", "BILLING_DISABLED", "API_KEY_INVALID", "ACCESS_TOKEN_EXPIRED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "SERVICE_DISABLED", "CREDENTIALS_MISSING", "CREDENTIALS_INVALID", "ACCESS_TOKEN_INVALID", "ACCESS_TOKEN_TYPE_UNSUPPORTED", "ACCOUNT_STATE_INVALID":
			return false
		case "RATE_LIMIT_EXCEEDED":
			rateLimited = true
		}
		if metadata, ok := value["metadata"].(map[string]any); ok {
			for _, key := range []string{"quota_limit", "quota_limit_value", "quota_metric"} {
				text, _ := metadata[key].(string)
				text = strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(text))
				if strings.Contains(text, "perday") || strings.Contains(text, "daily") {
					return false
				}
			}
		}
	}
	return rateLimited
}

func serverRetryDelay(err error, now time.Time) time.Duration {
	apiErr, ok := errors.AsType[*googleapi.Error](err)
	if !ok {
		return 0
	}
	var delay time.Duration
	if header := apiErr.Header.Get("Retry-After"); header != "" {
		if seconds, err := strconv.ParseUint(header, 10, 32); err == nil {
			delay = time.Duration(seconds) * time.Second
		} else if deadline, err := http.ParseTime(header); err == nil {
			delay = max(0, deadline.Sub(now))
		}
	}
	for _, detail := range apiErr.Details {
		value, ok := detail.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := value["@type"].(string)
		if strings.HasSuffix(kind, "/google.rpc.RetryInfo") {
			text, _ := value["retryDelay"].(string)
			if retry, err := time.ParseDuration(text); err == nil {
				delay = max(delay, retry)
			}
		}
	}
	return delay
}
