package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

func TestNotificationRetract(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/retract", 202, `{"devices":3}`)
	r := runCLI(t, srv, nil, "", "notification", "retract", "garage-door")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := mustJSON(t, f.calls[0].Body); got != `{"collapse_id":"garage-door"}` {
		t.Errorf("body %s", got)
	}
	if strings.TrimSpace(r.stdout) != `{"devices":3}` {
		t.Errorf("piped output should be the raw response, got %q", r.stdout)
	}

	for body, want := range map[string]string{
		`{"devices":3}`: "retracted garage-door on 3 devices\n",
		`{"devices":1}`: "retracted garage-door on 1 device\n",
		`{"devices":0}`: "retracted garage-door on 0 devices\n",
	} {
		f.reply("POST /notifications/retract", 202, body)
		r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "notification", "retract", "garage-door")
		if r.code != 0 || r.stdout != want {
			t.Errorf("%s on a terminal: exit %d out %q, want %q", body, r.code, r.stdout, want)
		}
	}
}

// Past 10 a minute the server answers 429, which the client retries before
// giving up with exit 5; a key without notification rights gets 403, exit 3.
func TestNotificationRetractRefused(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/retract", 429, `{"status":429,"code":"rate_limit.exceeded","detail":"retract is limited to 10 a minute","retry_after_ms":6000}`)
	r := runCLI(t, srv, nil, "", "notification", "retract", "garage-door")
	if r.code != ExitRateLimited || len(f.calls) != 5 || !strings.Contains(r.stderr, "10 a minute") {
		t.Errorf("429: exit %d after %d calls, stderr %q", r.code, len(f.calls), r.stderr)
	}
	for _, d := range r.sleeps {
		if d != 6*time.Second {
			t.Errorf("retried after %s, not the server's retry_after_ms", d)
		}
	}

	f.calls = nil
	f.reply("POST /notifications/retract", 403, `{"status":403,"code":"key.permission_denied","detail":"this key cannot send notifications"}`)
	r = runCLI(t, srv, nil, "", "notification", "retract", "garage-door")
	if r.code != ExitAuth || len(f.calls) != 1 {
		t.Errorf("403: exit %d after %d calls (%s)", r.code, len(f.calls), r.stderr)
	}
}

func TestNotificationAnswersPage(t *testing.T) {
	f, srv := newFake(t)
	var queries []string
	f.routes["GET /notifications/answers"] = func(c call) (int, string) {
		queries = append(queries, c.Query)
		return 200, `{"items":[],"cursor":"57"}`
	}
	for _, argv := range [][]string{
		{"notification", "answers"},
		{"notification", "answers", "--after", "57", "--limit", "10"},
	} {
		r := runCLI(t, srv, nil, "", argv...)
		if r.code != 0 || strings.TrimSpace(r.stdout) != `{"items":[],"cursor":"57"}` {
			t.Errorf("%v: exit %d out %q err %q", argv, r.code, r.stdout, r.stderr)
		}
	}
	if strings.Join(queries, " | ") != " | after=57&limit=10" {
		t.Errorf("queries %q", queries)
	}
	for _, limit := range []string{"0", "-1", "101"} {
		if r := runCLI(t, srv, nil, "", "notification", "answers", "--limit", limit); r.code != ExitUsage {
			t.Errorf("--limit %s: exit %d", limit, r.code)
		}
	}
	if len(queries) != 2 {
		t.Errorf("a bad --limit was sent: %q", queries)
	}
}

// Without --after, --wait starts from the current cursor and holds each
// request until an answer lands after it.
func TestNotificationAnswersWait(t *testing.T) {
	f, srv := newFake(t)
	var queries []string
	f.routes["GET /notifications/answers"] = func(c call) (int, string) {
		queries = append(queries, c.Query)
		if len(queries) < 3 {
			return 200, `{"items":[],"cursor":"57"}`
		}
		return 200, `{"items":[{"notification_id":9,"status":"answered","action_id":"go","collapse_id":"deploy"}],"cursor":"58"}`
	}
	r := runCLI(t, srv, nil, "", "notification", "answers", "--wait", "10m", "--jq", ".items[0].action_id")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "go" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	want := []string{"", "after=57&wait=25", "after=57&wait=25"}
	if strings.Join(queries, " | ") != strings.Join(want, " | ") {
		t.Errorf("queries %q, want %q", queries, want)
	}

	f.reply("GET /notifications/answers", 200, `{"items":[],"cursor":"57"}`)
	r = runCLI(t, srv, nil, "", "notification", "answers", "--after", "57", "--wait", "1m")
	if r.code != ExitWaitTimeout {
		t.Errorf("no answer in time: exit %d, want %d (%s)", r.code, ExitWaitTimeout, r.stderr)
	}
}

// --follow prints each page with answers as one JSON line, rides out a full
// set of wait slots, and on the way out says where to resume.
func TestNotificationAnswersFollow(t *testing.T) {
	f, srv := newFake(t)
	var queries []string
	f.routes["GET /notifications/answers"] = func(c call) (int, string) {
		queries = append(queries, c.Query)
		switch len(queries) {
		case 1:
			return 200, `{"items":[],"cursor":"57"}`
		case 2:
			return 200, "{\n  \"items\": [{\"notification_id\": 9, \"status\": \"answered\", \"action_id\": \"go\"}],\n  \"cursor\": \"58\"\n}"
		case 3:
			return 429, `{"status":429,"code":"answer_wait.limit_exceeded","detail":"too many concurrent waits; retry shortly"}`
		case 4:
			return 200, `{"items":[],"cursor":"58"}`
		case 5:
			return 200, `{"items":[{"notification_id":10,"status":"answered","action_id":"skip"}],"cursor":"59"}`
		}
		return 401, `{"status":401,"code":"token.invalid","detail":"invalid token"}`
	}
	r := runCLI(t, srv, nil, "", "notification", "answers", "--follow")
	if r.code != ExitAuth {
		t.Fatalf("exit %d, want %d once the key stops working (%s)", r.code, ExitAuth, r.stderr)
	}
	wantOut := `{"items":[{"notification_id":9,"status":"answered","action_id":"go"}],"cursor":"58"}` + "\n" +
		`{"items":[{"notification_id":10,"status":"answered","action_id":"skip"}],"cursor":"59"}` + "\n"
	if r.stdout != wantOut {
		t.Errorf("stdout\n got  %q\n want %q", r.stdout, wantOut)
	}
	if !strings.Contains(r.stderr, "resume with --after 59") {
		t.Errorf("stderr %q should name the cursor to resume from", r.stderr)
	}
	want := []string{"", "after=57&wait=25", "after=58&wait=25", "after=58&wait=25", "after=58&wait=25", "after=59&wait=25"}
	if strings.Join(queries, " | ") != strings.Join(want, " | ") {
		t.Errorf("queries\n got  %q\n want %q", queries, want)
	}
	// The full wait slots back off the first step of the retry backoff, the
	// empty page that came back at once 2s.
	if len(r.sleeps) != 2 || r.sleeps[0] <= 1500*time.Millisecond || r.sleeps[0] > 2*time.Second || r.sleeps[1] != 2*time.Second {
		t.Errorf("sleeps %v", r.sleeps)
	}

	if r := runCLI(t, srv, nil, "", "notification", "answers", "--follow", "--wait", "1m"); r.code != ExitUsage {
		t.Errorf("--follow with --wait: exit %d", r.code)
	}
}

// answerFeed answers GET /notifications/answers with replies[n-1] on the
// n-th request, and with the last reply after that. It returns the number
// of requests so far.
func answerFeed(f *fakeAPI, replies ...func() (int, string)) func() int {
	var mu sync.Mutex
	n := 0
	f.routes["GET /notifications/answers"] = func(call) (int, string) {
		mu.Lock()
		n++
		i := min(n, len(replies)) - 1
		mu.Unlock()
		return replies[i]()
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func times(k, status int, body string) []func() (int, string) {
	out := make([]func() (int, string), k)
	for i := range out {
		out[i] = func() (int, string) { return status, body }
	}
	return out
}

func page(body string) func() (int, string) {
	return func() (int, string) { return 200, body }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const (
	waitLimited = `{"status":429,"code":"answer_wait.limit_exceeded","detail":"too many concurrent waits; retry shortly"}`
	rateLimited = `{"status":429,"code":"rate_limit.exceeded","detail":"slow down"}`
	quotaUsedUp = `{"status":429,"code":"quota.exceeded","detail":"monthly quota used up"}`
	tokenBad    = `{"status":401,"code":"token.invalid","detail":"invalid token"}`
)

// retryWaits reads the waits --follow announced on stderr.
func retryWaits(t *testing.T, stderr string) []time.Duration {
	t.Helper()
	var waits []time.Duration
	for line := range strings.SplitSeq(stderr, "\n") {
		_, d, ok := strings.Cut(line, "; retrying in ")
		if !ok {
			continue
		}
		w, err := time.ParseDuration(d)
		if err != nil {
			t.Fatalf("retry line %q: %v", line, err)
		}
		waits = append(waits, w)
	}
	return waits
}

// --follow waits out a dropped connection, a 502 and rate limits, backing
// off from 2s to 30s, starts over once a page comes back, and stops on a
// used-up quota.
func TestNotificationAnswersFollowRetries(t *testing.T) {
	f, srv := newFake(t)
	replies := []func() (int, string){page(`{"items":[],"cursor":"57"}`)}
	// The client tries a GET five times before follow sees the error.
	replies = append(replies, times(5, 502, "bad gateway")...)
	replies = append(replies, times(5, 429, rateLimited)...)
	replies = append(replies, times(4, 429, waitLimited)...)
	replies = append(replies, page(`{"items":[{"notification_id":9,"action_id":"go"}],"cursor":"58"}`))
	replies = append(replies, times(1, 429, waitLimited)...)
	replies = append(replies, times(1, 429, quotaUsedUp)...)
	calls := answerFeed(f, replies...)

	var mu sync.Mutex
	dropped := 0
	r := runWith(t, srv, func(a *App) {
		next := srv.Client().Transport
		a.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			drop := req.URL.Query().Has("wait") && dropped < 5
			if drop {
				dropped++
			}
			mu.Unlock()
			if drop {
				return nil, errors.New("connection reset by peer")
			}
			return next.RoundTrip(req)
		})}
	}, "notification", "answers", "--follow")

	if r.code != ExitRateLimited {
		t.Fatalf("exit %d, want %d on a used-up quota (%s)", r.code, ExitRateLimited, r.stderr)
	}
	if want := `{"items":[{"notification_id":9,"action_id":"go"}],"cursor":"58"}` + "\n"; r.stdout != want {
		t.Errorf("stdout %q, want %q", r.stdout, want)
	}
	if n := calls(); n != len(replies) {
		t.Errorf("%d requests reached the server, want %d", n, len(replies))
	}
	for _, s := range []string{"connection reset by peer", ": 502", "rate_limit.exceeded", "quota.exceeded", "resume with --after 58"} {
		if !strings.Contains(r.stderr, s) {
			t.Errorf("stderr should mention %q:\n%s", s, r.stderr)
		}
	}
	steps := []time.Duration{2, 4, 8, 16, 30, 30, 30, 2}
	waits := retryWaits(t, r.stderr)
	if len(waits) != len(steps) {
		t.Fatalf("retried %d times, want %d:\n%s", len(waits), len(steps), r.stderr)
	}
	for i, w := range waits {
		top := steps[i] * time.Second
		if w < top*3/4 || w > top {
			t.Errorf("retry %d waited %s, want up to a quarter under %s", i, w, top)
		}
	}

	// -q keeps the retries and the resume hint off stderr.
	replies = append([]func() (int, string){page(`{"items":[],"cursor":"57"}`)}, times(5, 502, "bad gateway")...)
	answerFeed(f, append(replies, times(1, 401, tokenBad)...)...)
	r = runCLI(t, srv, nil, "", "notification", "answers", "--follow", "-q")
	if r.code != ExitAuth || strings.Contains(r.stderr, "retrying") || strings.Contains(r.stderr, "resume") {
		t.Errorf("-q: exit %d stderr %q", r.code, r.stderr)
	}
}

func TestFollowBackoff(t *testing.T) {
	for n, top := range []time.Duration{2, 4, 8, 16, 30, 30, 30} {
		top *= time.Second
		for range 50 {
			if d := followBackoff(n, errors.New("reset")); d <= top*3/4 || d > top {
				t.Fatalf("failure %d: waited %s, want up to a quarter under %s", n, d, top)
			}
		}
	}
	if d := followBackoff(1000, nil); d > followRetryMax {
		t.Errorf("a long outage waits %s", d)
	}
	// The server asking for longer wins, up to the cap.
	slow := &api.Error{Status: 429, Code: api.CodeAnswerWaitLimit, RetryAfterMs: 10_000}
	if d := followBackoff(0, slow); d != 10*time.Second {
		t.Errorf("retry_after_ms 10000: waited %s", d)
	}
	slow.RetryAfterMs = 600_000
	if d := followBackoff(0, slow); d != followRetryMax {
		t.Errorf("retry_after_ms 600000: waited %s", d)
	}
}

// An interrupt ends --follow with exit 0 and the cursor of the last page.
func TestNotificationAnswersFollowInterrupted(t *testing.T) {
	f, srv := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answerFeed(f,
		page(`{"items":[{"notification_id":9,"action_id":"go"}],"cursor":"58"}`),
		func() (int, string) {
			cancel()
			return 200, `{"items":[],"cursor":"58"}`
		},
	)
	r := runWith(t, srv, func(a *App) { a.Context = ctx }, "notification", "answers", "--follow", "--after", "57")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if r.stdout != `{"items":[{"notification_id":9,"action_id":"go"}],"cursor":"58"}`+"\n" {
		t.Errorf("stdout %q", r.stdout)
	}
	if strings.TrimSpace(r.stderr) != "resume with --after 58" {
		t.Errorf("stderr %q", r.stderr)
	}
}

// On a terminal --follow prints one line per answer and no cursor lines.
func TestNotificationAnswersFollowTTY(t *testing.T) {
	f, srv := newFake(t)
	answerFeed(f,
		page(`{"items":[{"notification_id":9,"action_id":"go"},{"notification_id":10,"action_id":"reply","text":"after lunch","collapse_id":"deploy"}],"cursor":"58"}`),
		func() (int, string) { return 401, tokenBad },
	)
	r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "notification", "answers", "--follow", "--after", "57")
	if r.code != ExitAuth {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if want := "notification 9: go\nnotification 10: reply (\"after lunch\"), collapse id deploy\n"; r.stdout != want {
		t.Errorf("stdout %q, want %q", r.stdout, want)
	}
	if !strings.Contains(r.stderr, "resume with --after 58") {
		t.Errorf("stderr %q", r.stderr)
	}
}

// No resume hint without a cursor to resume from: the first read failed, or
// the server refused the cursor the caller passed before any page came back
// for it.
func TestNotificationAnswersFollowNoHint(t *testing.T) {
	f, srv := newFake(t)
	answerFeed(f, func() (int, string) { return 401, tokenBad })
	r := runCLI(t, srv, nil, "", "notification", "answers", "--follow")
	if r.code != ExitAuth || strings.Contains(r.stderr, "resume") {
		t.Errorf("first read refused: exit %d stderr %q", r.code, r.stderr)
	}

	calls := answerFeed(f, func() (int, string) {
		return 400, `{"status":400,"code":"cursor.invalid","detail":"after is not a cursor of this stream"}`
	})
	r = runCLI(t, srv, nil, "", "notification", "answers", "--follow", "--after", "abc")
	if r.code != ExitError || calls() != 1 || strings.Contains(r.stderr, "resume") || !strings.Contains(r.stderr, "cursor.invalid") {
		t.Errorf("cursor refused: exit %d after %d calls, stderr %q", r.code, calls(), r.stderr)
	}

	// A cursor the server handed out is worth printing.
	answerFeed(f,
		page(`{"items":[],"cursor":"57"}`),
		func() (int, string) { return 404, `{"status":404,"code":"not_found","detail":"gone"}` },
	)
	r = runCLI(t, srv, nil, "", "notification", "answers", "--follow")
	if r.code != ExitNotFound || !strings.Contains(r.stderr, "resume with --after 57") {
		t.Errorf("404 after a cursor: exit %d stderr %q", r.code, r.stderr)
	}
}

func TestWriteAnswerPage(t *testing.T) {
	var b bytes.Buffer
	body := `{"items":[{"notification_id":9,"action_id":"reply","text":"after lunch","collapse_id":"deploy"},{"notification_id":10,"action_id":"go"}],"cursor":"58"}`
	if err := writeAnswerPage(&b, []byte(body), true); err != nil {
		t.Fatal(err)
	}
	want := "notification 9: reply (\"after lunch\"), collapse id deploy\nnotification 10: go\ncursor 58\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
	b.Reset()
	if err := writeAnswerPage(&b, []byte(`{"items":[],"cursor":"57"}`), true); err != nil || b.String() != "no new answers; cursor 57\n" {
		t.Errorf("empty page: %q %v", b.String(), err)
	}
}
