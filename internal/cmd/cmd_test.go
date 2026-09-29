package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Auth        string
	Body        map[string]any
}

// fakeAPI records requests and answers from a per-route handler.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []call
	routes map[string]func(c call) (int, string)
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type"), Auth: r.Header.Get("Authorization")}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &c.Body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h := f.routes[r.Method+" "+c.Path]
	f.mu.Unlock()
	if h == nil {
		w.WriteHeader(404)
		io.WriteString(w, `{"status":404,"code":"not_found","detail":"no route in fake"}`)
		return
	}
	status, body := h(c)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (f *fakeAPI) reply(route string, status int, body string) {
	f.routes[route] = func(call) (int, string) { return status, body }
}

type clock struct{ t time.Time }

func newFake(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{routes: map[string]func(call) (int, string){}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	t.Setenv("PUSHWARD_CONFIG_DIR", t.TempDir())
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_test")
	t.Setenv("PUSHWARD_API_URL", srv.URL)
	return f, srv
}

type result struct {
	stdout, stderr string
	code           int
	app            *App
	sleeps         []time.Duration
}

func runCLI(t *testing.T, srv *httptest.Server, env map[string]string, stdin string, argv ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	clk := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	var sleeps []time.Duration
	a := &App{
		Version: "test", Commit: "c", Date: "d",
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		HTTP: srv.Client(),
		Now:  func() time.Time { return clk.t },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			clk.t = clk.t.Add(d)
			return nil
		},
		Getenv: func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return os.Getenv(k)
		},
	}
	code := Execute(a, argv)
	return result{out.String(), errb.String(), code, a, sleeps}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNotifyBody(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":7,"delivery":"all","answerable":false}`)
	r := runCLI(t, srv, nil, "line one\nline two\n",
		"notify", "--title", "Build", "--body", "-", "--level", "time-sensitive",
		"--action", "ok=OK", "--meta", "run=12", "--image", "https://x/y.png", "--no-push",
		"-F", "volume=0.5", "-f", "source=ci")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	got := mustJSON(t, f.calls[0].Body)
	want := `{"actions":[{"id":"ok","title":"OK"}],"body":"line one\nline two","level":"time-sensitive","media":{"type":"image","url":"https://x/y.png"},"metadata":{"run":"12"},"push":false,"source":"ci","title":"Build","volume":0.5}`
	if got != want {
		t.Errorf("body\n got  %s\n want %s", got, want)
	}
	if f.calls[0].Auth != "Bearer hlk_test" {
		t.Errorf("auth %q", f.calls[0].Auth)
	}
	if strings.TrimSpace(r.stdout) != `{"id":7,"delivery":"all","answerable":false}` {
		t.Errorf("piped output should be the raw response, got %q", r.stdout)
	}
}

func TestNotifyWarnsOnPartialDelivery(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":7,"delivery":"none","reason":"no_apns_token"}`)
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b")
	if r.code != 0 || !strings.Contains(r.stderr, "delivery none (no_apns_token)") {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
}

func TestNotifyWaitsForAnswer(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":9,"answerable":true,"delivery":"all"}`)
	polls := 0
	f.routes["GET /notifications/answers/9"] = func(c call) (int, string) {
		polls++
		if c.Query != "wait=25" {
			t.Errorf("poll query %q", c.Query)
		}
		if polls < 3 {
			return 200, `{"notification_id":9,"status":"pending"}`
		}
		return 200, `{"notification_id":9,"status":"answered","action_id":"deploy"}`
	}
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--action", "deploy=Deploy", "--wait", "10m", "--jq", ".action_id")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "deploy" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	if polls != 3 {
		t.Errorf("answered after %d polls, want 3", polls)
	}
}

func TestNotifyWaitTimesOut(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":9,"answerable":true}`)
	f.reply("GET /notifications/answers/9", 200, `{"notification_id":9,"status":"pending"}`)
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--action", "x=X", "--wait", "1m")
	if r.code != ExitWaitTimeout {
		t.Errorf("exit %d, want %d (%s)", r.code, ExitWaitTimeout, r.stderr)
	}
}

func TestNotifyWaitNeedsAnswerable(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":9,"answerable":false}`)
	// No action at all: refused before anything is pushed.
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--wait", "1m")
	if r.code != ExitUsage || len(f.calls) != 0 {
		t.Errorf("exit %d after %d calls", r.code, len(f.calls))
	}
	// Only url actions: the server says it cannot be answered.
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--wait", "1m", "-F", "actions[0].url=https://x")
	if r.code != ExitUsage || len(f.calls) != 1 {
		t.Errorf("exit %d after %d calls", r.code, len(f.calls))
	}
}

func TestJQCheckedBeforeRequest(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":1}`)
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--jq", ".id |")
	if r.code == 0 || len(f.calls) != 0 {
		t.Errorf("bad --jq sent the notification anyway: exit %d, %d calls", r.code, len(f.calls))
	}
}

func TestEmptyUpdateRefused(t *testing.T) {
	f, srv := newFake(t)
	for _, argv := range [][]string{{"activity", "update", "a"}, {"widget", "update", "w"}} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d", argv, r.code)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("empty updates reached the API: %+v", f.calls)
	}
}

func TestActivityStart(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /activities", 201, `{"slug":"deploy","state":"ended"}`)
	f.reply("PATCH /activities/deploy", 200, `{"slug":"deploy","state":"ongoing"}`)
	r := runCLI(t, srv, nil, "", "activity", "start", "deploy", "--name", "Deploy", "--stale-ttl", "6h",
		"--text", "Building", "--step", "1/3", "--step-labels", "Build,Test,Ship", "--eta", "20m")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := mustJSON(t, f.calls[0].Body); got != `{"name":"Deploy","slug":"deploy","stale_ttl":21600}` {
		t.Errorf("create body %s", got)
	}
	eta := time.Date(2026, 9, 29, 12, 20, 0, 0, time.UTC).Unix()
	want := `{"content":{"current_step":1,"end_date":` + jsonInt(eta) + `,"state":"Building","step_labels":["Build","Test","Ship"],"template":"generic","total_steps":3},"state":"ongoing"}`
	if got := mustJSON(t, f.calls[1].Body); got != want {
		t.Errorf("patch body\n got  %s\n want %s", got, want)
	}
	if f.calls[1].ContentType != "application/merge-patch+json" {
		t.Errorf("content type %s", f.calls[1].ContentType)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestActivityEndSuccessTwoPhase(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /activities/build", 200, `{"slug":"build","state":"ongoing","content":{"template":"steps","total_steps":4,"current_step":2,"live_progress":true}}`)
	f.reply("PATCH /activities/build", 200, `{"slug":"build","state":"ended"}`)
	r := runCLI(t, srv, nil, "", "activity", "end", "build", "--status", "success", "--dismiss-after", "10m")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls %d", len(f.calls))
	}
	first := `{"content":{"accent_color":"green","current_step":4,"icon":"checkmark.circle.fill","live_progress":null,"progress":1,"remaining_time":null,"state":"Success","template":"steps"},"state":"ongoing"}`
	if got := mustJSON(t, f.calls[1].Body); got != first {
		t.Errorf("final frame\n got  %s\n want %s", got, first)
	}
	if got := mustJSON(t, f.calls[2].Body); got != `{"dismissal_ttl":600,"state":"ended"}` {
		t.Errorf("end body %s", got)
	}
	if len(r.sleeps) != 1 || r.sleeps[0] != 4*time.Second {
		t.Errorf("display time %v", r.sleeps)
	}
}

func TestActivityEndApprovalKeepsQuestion(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /activities/ask", 200, `{"slug":"ask","state":"ongoing","content":{"template":"approval","state":"Ship it?","icon":"questionmark"}}`)
	f.reply("PATCH /activities/ask", 200, `{"slug":"ask","state":"ended"}`)
	r := runCLI(t, srv, nil, "", "activity", "end", "ask", "--status", "failure")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	// Nothing about the card changes, so it ends in one patch.
	if len(f.calls) != 2 || mustJSON(t, f.calls[1].Body) != `{"state":"ended"}` {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestActivityEndUnchangedFrameIsOnePatch(t *testing.T) {
	f, srv := newFake(t)
	// Already showing the failure; only the progress animation is left to stop.
	f.reply("GET /activities/a", 200, `{"slug":"a","state":"ongoing","content":{"template":"generic","state":"Failed","icon":"xmark.circle.fill","accent_color":"red","live_progress":true}}`)
	f.reply("PATCH /activities/a", 200, `{"slug":"a","state":"ended"}`)
	r := runCLI(t, srv, nil, "", "activity", "end", "a", "--status", "failure")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := `{"content":{"accent_color":"red","icon":"xmark.circle.fill","live_progress":null,"remaining_time":null,"state":"Failed","template":"generic"},"state":"ended"}`
	if len(f.calls) != 2 || mustJSON(t, f.calls[1].Body) != want || len(r.sleeps) != 0 {
		t.Errorf("calls %d sleeps %v body %s", len(f.calls), r.sleeps, mustJSON(t, f.calls[len(f.calls)-1].Body))
	}
}

func TestActivityEndNoDisplayTime(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /activities/a", 200, `{"slug":"a","state":"ongoing","content":{"template":"generic","state":"Working"}}`)
	f.reply("PATCH /activities/a", 200, `{"slug":"a","state":"ended"}`)
	r := runCLI(t, srv, nil, "", "activity", "end", "a", "--status", "cancelled", "--display-time", "0", "--text", "Stopped")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := `{"content":{"accent_color":"#8E8E93","icon":"stop.circle.fill","state":"Stopped","template":"generic"},"state":"ended"}`
	if len(f.calls) != 2 || mustJSON(t, f.calls[1].Body) != want {
		t.Errorf("got %s", mustJSON(t, f.calls[len(f.calls)-1].Body))
	}
}

func TestActivityEndMissing(t *testing.T) {
	_, srv := newFake(t)
	r := runCLI(t, srv, nil, "", "activity", "end", "gone")
	if r.code != ExitNotFound {
		t.Errorf("exit %d", r.code)
	}
	r = runCLI(t, srv, nil, "", "activity", "end", "gone", "--ignore-missing")
	if r.code != 0 || !strings.Contains(r.stderr, "does not exist") {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
}

func TestActivityWait(t *testing.T) {
	f, srv := newFake(t)
	n := 0
	f.routes["GET /activities/rel"] = func(call) (int, string) {
		n++
		if n == 1 {
			return 200, `{"slug":"rel","state":"ongoing","content":{"template":"approval"}}`
		}
		return 200, `{"slug":"rel","state":"ongoing","content":{"template":"approval","answer":{"option":"ship","by":"user"}}}`
	}
	r := runCLI(t, srv, nil, "", "activity", "wait", "rel", "--jq", ".content.answer.option")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "ship" {
		t.Errorf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
}

func TestActivityListAll(t *testing.T) {
	f, srv := newFake(t)
	f.routes["GET /activities"] = func(c call) (int, string) {
		if strings.Contains(c.Query, "after=p2") {
			return 200, `{"items":[{"slug":"c"}]}`
		}
		return 200, `{"items":[{"slug":"a"},{"slug":"b"}],"next_cursor":"p2"}`
	}
	r := runCLI(t, srv, nil, "", "activity", "list", "--all", "--state", "ongoing", "--jq", "[.items[].slug] | join(\",\")")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "a,b,c" {
		t.Errorf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	if q := f.calls[0].Query; !strings.Contains(q, "state=ongoing") || !strings.Contains(q, "limit=100") {
		t.Errorf("query %q", q)
	}
}

func TestWidgetUpdateAlwaysSendsContent(t *testing.T) {
	f, srv := newFake(t)
	f.reply("PATCH /widgets/cov", 200, `{"slug":"cov"}`)
	runCLI(t, srv, nil, "", "widget", "update", "cov", "--name", "Coverage")
	if got := mustJSON(t, f.calls[0].Body); got != `{"content":{},"name":"Coverage"}` {
		t.Errorf("body %s", got)
	}
}

func TestScheduleCreate(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/scheduled", 201, `{"id":3,"status":"scheduled","send_at":"2026-09-29T14:00:00Z"}`)
	r := runCLI(t, srv, nil, "", "schedule", "create", "--in", "2h", "--title", "t", "--body", "b")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := f.calls[0].Body["send_at"]; got != "2026-09-29T14:00:00Z" {
		t.Errorf("send_at %v", got)
	}
	r = runCLI(t, srv, nil, "", "schedule", "create", "--cron", "0 9 * * *", "--title", "t", "--body", "b")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--tz") {
		t.Errorf("cron without tz: exit %d %q", r.code, r.stderr)
	}
	r = runCLI(t, srv, nil, "", "schedule", "create", "--cron", "@daily", "--tz", "UTC", "--count", "3", "--title", "t", "--body", "b")
	if r.code != 0 || mustJSON(t, f.calls[len(f.calls)-1].Body["recurrence"]) != `{"count":3,"cron":"@daily","timezone":"UTC"}` {
		t.Errorf("recurrence: exit %d %v", r.code, f.calls[len(f.calls)-1].Body)
	}
}

func TestAPICommand(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /activities", 200, `{"items":[]}`)
	f.reply("PATCH /activities/x", 200, `{}`)
	runCLI(t, srv, nil, "", "api", "-X", "GET", "/activities?limit=5", "-f", "state=ongoing")
	if f.calls[0].Query != "limit=5&state=ongoing" || f.calls[0].Body != nil {
		t.Errorf("GET fields should become query: %+v", f.calls[0])
	}
	runCLI(t, srv, nil, "", "api", "-X", "PATCH", "activities/x", "-F", "content.progress=0.5")
	if got := mustJSON(t, f.calls[1].Body); got != `{"content":{"progress":0.5}}` {
		t.Errorf("body %s", got)
	}
	r := runCLI(t, srv, nil, "", "api", "https://evil.example/x")
	if r.code != ExitUsage {
		t.Errorf("full URL accepted: exit %d", r.code)
	}
}

func TestExitCodes(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /auth/me", 401, `{"status":401,"detail":"invalid key"}`)
	f.reply("POST /notifications", 429, `{"status":429,"code":"quota.exceeded","detail":"monthly limit"}`)
	for _, tc := range []struct {
		argv []string
		code int
	}{
		{[]string{"me"}, ExitAuth},
		{[]string{"notify", "--title", "a", "--body", "b"}, ExitRateLimited},
		{[]string{"activity", "get", "missing"}, ExitNotFound},
		{[]string{"activity", "get", "bad slug"}, ExitUsage},
		{[]string{"activity", "nope"}, ExitUsage},
		{[]string{"nope"}, ExitUsage},
		{[]string{"notfy"}, ExitUsage},
		{[]string{"notify", "--bogus"}, ExitUsage},
		{[]string{"activity", "update", "a", "--progress", "lots"}, ExitUsage},
		{[]string{"activity", "get"}, ExitUsage},
	} {
		if r := runCLI(t, srv, nil, "", tc.argv...); r.code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.argv, r.code, tc.code, r.stderr)
		}
	}
}

func TestNoToken(t *testing.T) {
	_, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	if r := runCLI(t, srv, nil, "", "me"); r.code != ExitAuth || !strings.Contains(r.stderr, "auth login") {
		t.Errorf("exit %d %q", r.code, r.stderr)
	}
}

func TestAuthLoginStoresKey(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	f.routes["GET /auth/me"] = func(c call) (int, string) {
		if c.Auth != "Bearer hlk_new" {
			return 401, `{"detail":"bad key"}`
		}
		return 200, `{"id":"u1","nickname":"Maciej"}`
	}
	r := runCLI(t, srv, nil, "hlk_new\n", "auth", "login", "--with-token", "--api-url", srv.URL)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("PUSHWARD_CONFIG_DIR"), "config.json"))
	if err != nil || !strings.Contains(string(data), `"token": "hlk_new"`) {
		t.Fatalf("config %s %v", data, err)
	}
	if r := runCLI(t, srv, nil, "", "me"); r.code != 0 {
		t.Errorf("stored key not used: exit %d %s", r.code, r.stderr)
	}
	if r := runCLI(t, srv, nil, "hlk_bad\n", "auth", "login", "--with-token"); r.code != ExitAuth {
		t.Errorf("bad key: exit %d", r.code)
	}
}

func TestHealthNeedsNoKey(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	f.reply("GET /health", 200, `{"status":"ok"}`)
	if r := runCLI(t, srv, nil, "", "health"); r.code != 0 || f.calls[0].Auth != "" {
		t.Errorf("exit %d auth %q err %s", r.code, f.calls[0].Auth, r.stderr)
	}
}
