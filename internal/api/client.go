// Package api is a thin client for the PushWard REST API. Bodies go out and
// come back as raw JSON, so fields the server adds pass through without a
// client release.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.pushward.app"
	defaultTimeout = 30 * time.Second
	// retryBudget bounds the total time one call spends across retries.
	retryBudget   = 60 * time.Second
	maxAttempts   = 5
	maxRetryAfter = 2 * time.Minute
	// longPollSlack is added to a ?wait= long-poll's own duration, so the
	// per-attempt timeout never fires before the server answers.
	longPollSlack = 15 * time.Second
	// A log activity with its backlog, or an --all listing page of timeline
	// activities, runs to a few hundred KiB. 8 MiB leaves room without letting
	// a misbehaving proxy fill memory.
	maxResponseBytes = 8 << 20
)

type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
	// Sleep waits between retries; nil means SleepContext. Tests swap it.
	Sleep func(context.Context, time.Duration) error
	// Debug, when set, receives one line per attempt: method, URL, status and
	// latency. Never headers or bodies: the token lives in one, and action
	// webhook headers can carry secrets in the other.
	Debug io.Writer
}

type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	Header http.Header
	// Timeout applies per attempt. Zero means 30s, or for a ?wait= long-poll
	// the wait plus slack.
	Timeout time.Duration
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Call runs an operation from the table with its path parameters filled in.
// It is the only way the CLI's commands reach the API.
func (c *Client) Call(ctx context.Context, opID string, params map[string]string, query url.Values, body []byte) (*Response, error) {
	op := Op(opID)
	path, err := op.Expand(params)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, Request{Method: op.Method, Path: path, Query: query, Body: body})
}

// Do sends a request, retrying only where a retry cannot duplicate an effect.
// 429 and 503 mean the server did not process the request, so every method
// retries them. Other 5xx and transport errors retry only for GET and DELETE:
// a replayed POST /notifications pushes twice, and a replayed PATCH on a
// timeline activity appends its data point twice.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if req.Timeout <= 0 {
		req.Timeout = defaultTimeout
		if w, err := strconv.Atoi(req.Query.Get("wait")); err == nil && w > 0 {
			req.Timeout = time.Duration(w)*time.Second + longPollSlack
		}
	}
	idempotent := req.Method == http.MethodGet || req.Method == http.MethodDelete || req.Method == http.MethodHead

	start := time.Now()
	var lastErr error
	for attempt := 1; ; attempt++ {
		resp, err := c.once(ctx, req)
		var delay time.Duration
		switch {
		case err != nil:
			lastErr = &TransportError{Method: req.Method, Path: req.Path, Attempts: attempt, Err: err}
			if ctx.Err() != nil || !idempotent {
				return nil, lastErr
			}
		case resp.Status >= 200 && resp.Status < 300:
			return resp, nil
		default:
			apiErr := newError(req.Method, req.Path, resp.Status, resp.Body)
			if !retryable(resp.Status, apiErr.Code, idempotent) {
				return nil, apiErr
			}
			lastErr = apiErr
			delay = serverDelay(resp.Header.Get("Retry-After"), apiErr.RetryAfterMs)
		}

		if attempt >= maxAttempts {
			return nil, lastErr
		}
		if delay == 0 {
			base := min(time.Second<<(attempt-1), 30*time.Second)
			delay = base/2 + rand.N(base/2) // #nosec G404 -- retry jitter, not security-sensitive
		}
		if time.Since(start)+delay > retryBudget {
			return nil, lastErr
		}
		if c.Debug != nil {
			fmt.Fprintf(c.Debug, "  retrying in %s\n", delay.Round(time.Millisecond))
		}
		sleep := c.Sleep
		if sleep == nil {
			sleep = SleepContext
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func retryable(status int, code string, idempotent bool) bool {
	switch {
	case status == http.StatusTooManyRequests:
		// quota.exceeded is a monthly cap, and answer_wait.limit_exceeded means
		// the long-poll slots are full: waiting inside this call fixes neither.
		return code != CodeQuotaExceeded && code != CodeAnswerWaitLimit
	case status == http.StatusServiceUnavailable:
		return true
	case status >= 500:
		return idempotent
	}
	return false
}

func (c *Client) once(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	u := strings.TrimRight(c.BaseURL, "/") + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if req.Body != nil && hr.Header.Get("Content-Type") == "" {
		ct := "application/json"
		if req.Method == http.MethodPatch {
			ct = "application/merge-patch+json"
		}
		hr.Header.Set("Content-Type", ct)
	}
	hr.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		hr.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.Token)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	t0 := time.Now()
	resp, err := httpClient.Do(hr)
	if err != nil {
		if c.Debug != nil {
			fmt.Fprintf(c.Debug, "%s %s: %v\n", req.Method, u, err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if c.Debug != nil {
		fmt.Fprintf(c.Debug, "%s %s: %d (%s)\n", req.Method, u, resp.StatusCode, time.Since(t0).Round(time.Millisecond))
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// SleepContext waits for d or until ctx is done.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// serverDelay reads the server's retry hint: Retry-After first (seconds or an
// HTTP date), then the Problem body's retry_after_ms. Clamped so a bad value
// cannot park the process for hours. Zero means no hint.
func serverDelay(header string, retryAfterMs int64) time.Duration {
	var d time.Duration
	if header != "" {
		if secs, err := strconv.ParseInt(header, 10, 64); err == nil {
			if secs > 0 {
				d = time.Duration(min(secs, int64(maxRetryAfter/time.Second))) * time.Second
			}
		} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(header, "-") {
			d = maxRetryAfter
		} else if t, err := http.ParseTime(header); err == nil {
			d = time.Until(t)
		}
	}
	if d <= 0 && retryAfterMs > 0 {
		d = time.Duration(min(retryAfterMs, maxRetryAfter.Milliseconds())) * time.Millisecond
	}
	return min(max(d, 0), maxRetryAfter)
}
