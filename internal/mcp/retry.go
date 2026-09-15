package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Upstreams throttle bursts (Hermes issues tool calls in parallel) with 429,
// and briefly return 503 during deploys. Neither status means the call was
// processed, so retrying is safe for non-idempotent tool calls too. 502 and
// 504 are not retried: the request may have reached the upstream.
const (
	retryAttempts = 4
	retryMaxWait  = 20 * time.Second
)

var retrySleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DoWithRetry sends the request, retrying 429 and 503 responses with the
// server's Retry-After when present and exponential backoff otherwise. The
// body must be supplied separately so it can be replayed.
func DoWithRetry(ctx context.Context, client *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) || attempt == retryAttempts-1 {
			return resp, nil
		}
		wait := retryAfter(resp.Header.Get("Retry-After"))
		if wait <= 0 {
			wait = time.Duration(1<<uint(attempt)) * time.Second
		}
		if wait > retryMaxWait {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err := retrySleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return time.Until(when)
	}
	return 0
}
