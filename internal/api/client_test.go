package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type step struct {
	status int
	header map[string]string
	body   string
}

// script serves the given responses in order and counts requests.
func script(t *testing.T, steps ...step) (*Client, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(steps) {
			t.Errorf("unexpected request %d", i+1)
			w.WriteHeader(500)
			return
		}
		for k, v := range steps[i].header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(steps[i].status)
		io.WriteString(w, steps[i].body)
	}))
	t.Cleanup(srv.Close)
	var sleeps []time.Duration
	c := &Client{BaseURL: srv.URL, Token: "hlk_test", HTTP: srv.Client()}
	c.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	return c, &n, &sleeps
}

func TestRetryMatrix(t *testing.T) {
	problem := func(code string) string { return `{"status":429,"code":"` + code + `","detail":"x"}` }
	for _, tc := range []struct {
		name     string
		method   string
		steps    []step
		requests int32
		ok       bool
	}{
		{"429 retried for POST", "POST", []step{{429, nil, problem("rate_limit.exceeded")}, {201, nil, `{}`}}, 2, true},
		{"quota fails fast", "POST", []step{{429, nil, problem(CodeQuotaExceeded)}}, 1, false},
		{"answer wait limit fails fast", "GET", []step{{429, nil, problem(CodeAnswerWaitLimit)}}, 1, false},
		{"503 retried for POST", "POST", []step{{503, nil, ""}, {201, nil, `{}`}}, 2, true},
		{"500 not retried for POST", "POST", []step{{500, nil, `{"detail":"boom"}`}}, 1, false},
		{"500 not retried for PATCH", "PATCH", []step{{502, nil, ""}}, 1, false},
		{"500 retried for GET", "GET", []step{{500, nil, ""}, {502, nil, ""}, {200, nil, `{}`}}, 3, true},
		{"404 not retried", "GET", []step{{404, nil, `{"detail":"no"}`}}, 1, false},
		{"gives up after 5", "GET", []step{{503, nil, ""}, {503, nil, ""}, {503, nil, ""}, {503, nil, ""}, {503, nil, ""}}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n, _ := script(t, tc.steps...)
			_, err := c.Do(context.Background(), Request{Method: tc.method, Path: "/x", Body: []byte(`{}`)})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if got := n.Load(); got != tc.requests {
				t.Errorf("requests = %d, want %d", got, tc.requests)
			}
		})
	}
}

func TestRetryHonorsServerDelay(t *testing.T) {
	c, _, sleeps := script(t,
		step{429, map[string]string{"Retry-After": "3"}, `{}`},
		step{429, nil, `{"code":"rate_limit.exceeded","retry_after_ms":1500}`},
		step{200, nil, `{}`},
	)
	if _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if got := *sleeps; len(got) != 2 || got[0] != 3*time.Second || got[1] != 1500*time.Millisecond {
		t.Errorf("sleeps = %v", got)
	}
}

func TestRetryBudget(t *testing.T) {
	c, n, _ := script(t, step{429, map[string]string{"Retry-After": "90"}, `{}`})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/x"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 429 {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Errorf("a delay past the budget should stop at once, made %d requests", n.Load())
	}
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, method = r.Header.Clone(), r.Method
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "hlk_abc", UserAgent: "pushward-cli/test", HTTP: srv.Client()}
	if _, err := c.Call(context.Background(), "updateActivity", map[string]string{"slug": "a"}, nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if method != "PATCH" {
		t.Errorf("method %s", method)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer hlk_abc",
		"Content-Type":  "application/merge-patch+json",
		"User-Agent":    "pushward-cli/test",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestExpandEscapes(t *testing.T) {
	p, err := Op("getActivity").Expand(map[string]string{"slug": "../auth/me"})
	if err != nil {
		t.Fatal(err)
	}
	if p != "/activities/..%2Fauth%2Fme" {
		t.Errorf("path %s", p)
	}
	if _, err := Op("getActivity").Expand(nil); err == nil {
		t.Error("missing parameter should fail")
	}
}

func TestErrorMessage(t *testing.T) {
	e := newError("PATCH", "/activities/a", 422, []byte(`{"status":422,"code":"activity.content_invalid","detail":"validation failed","errors":[{"location":"body.content.progress","message":"must be at most 1","value":2}]}`))
	want := "PATCH /activities/a: 422 activity.content_invalid: validation failed\n  body.content.progress: must be at most 1 (got 2)"
	if e.Error() != want {
		t.Errorf("got  %q\nwant %q", e.Error(), want)
	}
	q := newError("POST", "/notifications", 429, []byte(`{"code":"quota.exceeded","detail":"monthly limit","kind":"notifications","used":1000,"limit":1000,"reset_at":"2026-11-01T00:00:00Z"}`))
	if !strings.Contains(q.Error(), "(notifications 1000/1000, resets 2026-11-01T00:00:00Z)") {
		t.Errorf("quota message: %s", q.Error())
	}
	html := newError("GET", "/x", 502, []byte("<html>bad gateway</html>"))
	if !strings.Contains(html.Error(), "502: <html>bad gateway</html>") {
		t.Errorf("non-problem body: %s", html.Error())
	}
}

func TestServerDelay(t *testing.T) {
	for _, tc := range []struct {
		header string
		ms     int64
		want   time.Duration
	}{
		{"", 0, 0},
		{"5", 0, 5 * time.Second},
		{"-5", 0, 0},
		{"999999", 0, maxRetryAfter},
		{"99999999999999999999", 0, maxRetryAfter},
		{"", 250, 250 * time.Millisecond},
		{"junk", 250, 250 * time.Millisecond},
		{"", 1 << 40, maxRetryAfter},
	} {
		if got := serverDelay(tc.header, tc.ms); got != tc.want {
			t.Errorf("serverDelay(%q, %d) = %v, want %v", tc.header, tc.ms, got, tc.want)
		}
	}
}
