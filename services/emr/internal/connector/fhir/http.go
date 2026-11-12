package fhir

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
)

// transportError is a failed request's closed code: a deadline is
// timed_out, a request the egress client refused before sending is
// sign_in_refused (design §6.3: nothing sent), an oversized answer is
// unreadable_data, anything else unreachable. The error's text (which may
// name the URL) is never kept.
func transportError(ctx context.Context, err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()):
		return connector.Fail("timed_out", 0)
	case errors.Is(err, egress.ErrOrigin):
		return connector.Fail("sign_in_refused", 0)
	case errors.Is(err, egress.ErrTooLarge):
		return connector.Fail("unreadable_data", 0)
	}
	return connector.Fail("unreachable", 0)
}

// statusError is a non-2xx answer's closed code (nil for 2xx).
func statusError(resp *http.Response, now time.Time) *connector.FetchError {
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return connector.Fail("sign_in_refused", 0)
	case s == http.StatusTooManyRequests:
		return connector.Fail("too_many_requests", retryAfter(resp.Header.Get("Retry-After"), now))
	case s >= 500:
		return connector.Fail("unreachable", 0)
	}
	return connector.Fail("unreadable_data", 0)
}

// retryAfter reads Retry-After as seconds or an HTTP date, capped at
// MaxRetryAfter; anything else (junk, negative, past) is 0: the caller's
// own backoff applies.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now).Round(time.Second)
	}
	if d <= 0 {
		return 0
	}
	return min(d, MaxRetryAfter)
}

// getJSON GETs u with the bearer token and reads one JSON document of at
// most egress.MaxBody bytes and egress.MaxDepth deep into v.
func (c *FHIR) getJSON(ctx context.Context, client *http.Client, u, token string, v any) error {
	rctx, cancel := pageContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return connector.Fail("unreadable_data", 0)
	}
	req.Header.Set("Accept", "application/fhir+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return transportError(rctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if fe := statusError(resp, c.now()); fe != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fe
	}
	if err := egress.DecodeJSON(resp.Body, v, egress.MaxDepth); err != nil {
		if rctx.Err() != nil {
			return connector.Fail("timed_out", 0)
		}
		return connector.Fail("unreadable_data", 0)
	}
	return nil
}
