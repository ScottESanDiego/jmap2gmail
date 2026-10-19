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

	"google.golang.org/api/googleapi"
)

// ClientOptions controls diagnostics for Gmail quota retries.
type ClientOptions struct {
	Diagnostics io.Writer
}

type requestPolicy struct {
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
	jitter func() time.Duration
	output io.Writer
	logMu  sync.Mutex
}

func newRequestPolicy(options ClientOptions) *requestPolicy {
	output := options.Diagnostics
	if output == nil {
		output = io.Discard
	}
	return &requestPolicy{
		now: time.Now, wait: waitContext,
		jitter: func() time.Duration { return rand.N(time.Second) }, output: output,
	}
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

func (p *requestPolicy) logWait(method string, delay time.Duration) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.output, "Gmail quota: rate limit; retrying %s, waiting %s\n", method, delay.Round(time.Millisecond))
}

func (p *requestPolicy) logRecovery(method string, retries int, elapsed time.Duration) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.output, "Gmail quota: recovered %s after %d quota retries (elapsed %s)\n", method, retries, elapsed.Round(time.Millisecond))
}

// execute surrounds a single fresh SDK call. Only explicit quota rejections
// delay and replay; waiting stays outside the HTTP client's attempt timeout.
func execute[T any](ctx context.Context, c *Client, method string, attempt func() (T, error)) (T, error) {
	var zero T
	c.policyOnce.Do(func() {
		if c.policy == nil {
			c.policy = newRequestPolicy(ClientOptions{})
		}
	})
	p := c.policy
	started := p.now()
	retries := 0
	hadQuota := false
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if hadQuota {
			retries++
		}
		result, err := attempt()
		if !isMinuteQuotaError(err) {
			if err == nil && hadQuota {
				p.logRecovery(method, retries, p.now().Sub(started))
			}
			return result, err
		}
		hadQuota = true
		delay := min(backoff+p.jitter(), 64*time.Second)
		delay = max(delay, serverRetryDelay(err, p.now()))
		p.logWait(method, delay)
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
