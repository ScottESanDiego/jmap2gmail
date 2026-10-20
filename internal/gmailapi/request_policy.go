package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/googleapi"
)

// ClientOptions controls diagnostics for Gmail retries.
type ClientOptions struct {
	Diagnostics io.Writer
}

type requestPolicy struct {
	attemptTimeout time.Duration
	now            func() time.Time
	wait           func(context.Context, time.Duration) error
	jitter         func() time.Duration
	output         io.Writer
	logMu          sync.Mutex
}

func newRequestPolicy(options ClientOptions) *requestPolicy {
	output := options.Diagnostics
	if output == nil {
		output = io.Discard
	}
	return &requestPolicy{
		attemptTimeout: 60 * time.Second, now: time.Now, wait: waitContext,
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

// execute bounds each fresh SDK call, including response decoding. Backoff
// uses the parent context, so waits do not consume the next attempt's deadline.
func execute[T any](ctx context.Context, c *Client, method string, attempt func(context.Context) (T, error)) (T, error) {
	var zero T
	c.policyOnce.Do(func() {
		if c.policy == nil {
			c.policy = newRequestPolicy(ClientOptions{})
		}
	})
	p := c.policy
	started := p.now()
	quotaRetries, timeoutRetries := 0, 0
	pending := ""
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		switch pending {
		case "quota":
			quotaRetries++
		case "timeout":
			timeoutRetries++
		}
		attemptCtx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
		result, err := attempt(attemptCtx)
		attemptErr := attemptCtx.Err()
		cancel()
		// A decoded successful response confirms even a write whose deadline
		// raced with completion. Return its ID so the engine can checkpoint it.
		if err == nil {
			if quotaRetries+timeoutRetries > 0 {
				if timeoutRetries == 0 {
					p.logRecovery(method, quotaRetries, p.now().Sub(started))
				} else {
					p.logMu.Lock()
					fmt.Fprintf(p.output, "Gmail retry: recovered %s after %d quota retries and %d timeout retries (elapsed %s)\n", method, quotaRetries, timeoutRetries, p.now().Sub(started).Round(time.Millisecond))
					p.logMu.Unlock()
				}
			}
			return result, nil
		}
		_, conclusiveHTTP := errors.AsType[*googleapi.Error](err)
		if parentErr := ctx.Err(); parentErr != nil {
			if conclusiveHTTP {
				return result, errors.Join(err, parentErr)
			}
			return zero, parentErr
		}
		if !conclusiveHTTP && errors.Is(attemptErr, context.DeadlineExceeded) && !isTimeoutError(err) {
			err = errors.Join(err, attemptErr)
		}
		quota := isMinuteQuotaError(err)
		timeout := !quota && !conclusiveHTTP && timeoutRetryMethod(method) && isTimeoutError(err)
		if !quota && !timeout {
			return result, err
		}
		delay := min(backoff+p.jitter(), 64*time.Second)
		if quota {
			pending = "quota"
			delay = max(delay, serverRetryDelay(err, p.now()))
			p.logWait(method, delay)
		} else {
			pending = "timeout"
			p.logMu.Lock()
			fmt.Fprintf(p.output, "Gmail timeout: retrying %s after attempt %d (elapsed %s), waiting %s\n", method, quotaRetries+timeoutRetries+1, p.now().Sub(started).Round(time.Millisecond), delay.Round(time.Millisecond))
			p.logMu.Unlock()
		}
		if err := p.wait(ctx, delay); err != nil {
			return zero, fmt.Errorf("wait to retry Gmail %s: %w", pending, err)
		}
		backoff = min(backoff*2, 64*time.Second)
	}
}

func timeoutRetryMethod(method string) bool {
	switch method {
	case "labels.list", "getProfile", "messages.get", "messages.list", "history.list":
		return true
	default:
		return false
	}
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if network, ok := err.(net.Error); ok && network.Timeout() {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(wrapped.Unwrap(), isTimeoutError)
	case interface{ Unwrap() error }:
		return isTimeoutError(wrapped.Unwrap())
	}
	return false
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
