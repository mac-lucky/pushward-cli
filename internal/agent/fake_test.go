package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

type call struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

// fakeAPI is PushWard as far as the agent sees it. It parks ?wait= reads the
// way the server does and records answers when the script says they come,
// on the fake clock of a synctest bubble.
type fakeAPI struct {
	mu    sync.Mutex
	calls []call

	// createStatus, when set, refuses POST /activities with that status,
	// code (plan.required when "") and detail, before the target is read.
	createStatus int
	createCode   string
	createDetail string
	// notifStatus, when set, refuses POST /notifications with that status
	// and code, before the target is read.
	notifStatus int
	notifCode   string
	// existing is the stored activity a create finds, "" for none.
	existing string
	// started is the devices_started a read reports, -1 to leave it out.
	started int
	// cardAnswer is the option tapped on the card at a step (0 on a card
	// with one question), and how long after the step was shown; "" for
	// never.
	cardAnswer func(step int) (string, time.Duration)
	// endCard ends the card this long after it starts, with no answer.
	endCard time.Duration
	// optionURL is the url the server would fill into each option.
	optionURL string
	// notifAnswer is the action tapped on the n-th notification (from 1),
	// the text typed with it, and how long after it was sent.
	notifAnswer func(n int, sent map[string]any) (string, string, time.Duration)
	// waitLimit answers every ?wait= read 429 answer_wait.limit_exceeded.
	waitLimit bool
	// status answers every request with it, when set.
	status int
	// org makes the key an organization's: the key check is refused as an
	// invalid target rather than one a personal key cannot have.
	org bool
	// probes counts the key checks, which calls leaves out.
	probes int
	// startStatus, when set, refuses the PATCH that starts the card.
	startStatus int
	// noSteps is a server that predates steps: any answer ends the card.
	noSteps bool

	act    *fakeActivity
	notifs []*fakeNotification
}

type fakeActivity struct {
	state   string
	content map[string]any
	since   time.Time // when the current round started
	created time.Time
}

type fakeNotification struct {
	sent   map[string]any
	at     time.Time
	action string
	text   string
}

func newFakeAPI() *fakeAPI { return &fakeAPI{started: 1} }

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	res := rec.Result()
	res.Request = r
	return res, nil
}

func problem(status int, code, detail string) (int, any) {
	return status, map[string]any{"status": status, "code": code, "detail": detail}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query()}
	if r.Body != nil {
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &c.Body)
		}
	}
	f.mu.Lock()
	if probe(c) {
		f.probes++
	} else {
		f.calls = append(f.calls, c)
	}
	f.mu.Unlock()
	status, body := f.route(r.Context(), c)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (f *fakeAPI) route(ctx context.Context, c call) (int, any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		return problem(f.status, "", "scripted")
	}
	wait, _ := strconv.Atoi(c.Query.Get("wait"))
	if wait > 0 && f.waitLimit {
		return problem(429, api.CodeAnswerWaitLimit, "too many waits")
	}
	switch {
	case c.Method == "POST" && c.Path == "/activities":
		if f.createStatus != 0 {
			code := f.createCode
			if code == "" {
				code = "plan.required"
			}
			return problem(f.createStatus, code, f.createDetail)
		}
		if probe(c) {
			return f.refuseTarget()
		}
		if f.act == nil {
			f.act = &fakeActivity{state: "ongoing", content: map[string]any{}, created: time.Now()}
			if f.existing != "" {
				_ = json.Unmarshal([]byte(f.existing), &f.act.content)
				f.act.state = "ended"
				f.existing = ""
			}
		}
		return 200, f.activityJSON()
	case c.Method == "PATCH" && strings.HasPrefix(c.Path, "/activities/"):
		if f.act == nil {
			return problem(404, "not_found", "no activity")
		}
		if f.startStatus != 0 && c.Body["state"] == "ongoing" {
			return problem(f.startStatus, api.CodeQuotaExceeded, "scripted")
		}
		if s, ok := c.Body["state"].(string); ok {
			f.act.state = s
		}
		if next, ok := c.Body["content"].(map[string]any); ok {
			prev := f.act.content
			merged := map[string]any{}
			for k, v := range prev {
				merged[k] = v
			}
			for k, v := range next {
				merged[k] = v
			}
			if !sameJSON(prev["options"], merged["options"]) || !sameJSON(prev["step"], merged["step"]) {
				delete(merged, "answer")
				f.act.since = time.Now()
			}
			f.act.content = merged
		}
		return 200, f.activityJSON()
	case c.Method == "DELETE" && strings.HasPrefix(c.Path, "/activities/"):
		f.act = nil
		return 204, nil
	case c.Method == "GET" && strings.HasPrefix(c.Path, "/activities/"):
		return f.park(ctx, wait, func() (bool, time.Time) {
			if f.act == nil {
				return true, time.Time{}
			}
			due := f.settleCard()
			return f.act.state != "ongoing" || f.act.content["answer"] != nil, due
		}, func() (int, any) {
			if f.act == nil {
				return problem(404, "not_found", "no activity")
			}
			return 200, f.activityJSON()
		})
	case c.Method == "POST" && c.Path == "/notifications":
		if f.notifStatus != 0 {
			return problem(f.notifStatus, f.notifCode, "scripted")
		}
		if probe(c) {
			return f.refuseTarget()
		}
		n := &fakeNotification{sent: c.Body, at: time.Now()}
		f.notifs = append(f.notifs, n)
		out := map[string]any{"id": len(f.notifs)}
		if c.Body["actions"] != nil {
			out["answerable"] = true
		}
		return 201, out
	case c.Method == "GET" && strings.HasPrefix(c.Path, "/notifications/answers/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(c.Path, "/notifications/answers/"))
		if id < 1 || id > len(f.notifs) {
			return problem(404, "not_found", "no notification")
		}
		n := f.notifs[id-1]
		return f.park(ctx, wait, func() (bool, time.Time) {
			return f.settleNotification(id, n)
		}, func() (int, any) {
			if n.action == "" {
				return 200, map[string]any{"notification_id": id, "status": "pending"}
			}
			return 200, map[string]any{"notification_id": id, "status": "answered", "action_id": n.action, "text": n.text}
		})
	case c.Method == "POST" && c.Path == "/notifications/retract":
		return 202, map[string]any{"devices": 1}
	}
	return problem(404, "not_found", "no route in fake")
}

// park answers at once when ready says so, else holds the read like a
// ?wait= long-poll: until the scripted answer is due, the wait runs out, or
// the reader gives up. f.mu is held on entry and on return.
func (f *fakeAPI) park(ctx context.Context, wait int, ready func() (bool, time.Time), respond func() (int, any)) (int, any) {
	end := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		done, due := ready()
		if done || !time.Now().Before(end) {
			return respond()
		}
		until := end
		if !due.IsZero() && due.Before(until) {
			until = due
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			f.mu.Lock()
			return 499, nil
		case <-time.After(time.Until(until)):
		}
		f.mu.Lock()
	}
}

// settleCard records the scripted answer or end once it is due, and
// returns when the next one is.
func (f *fakeAPI) settleCard() time.Time {
	a := f.act
	if a.state != "ongoing" || a.content["answer"] != nil {
		return time.Time{}
	}
	var due time.Time
	if f.endCard > 0 {
		due = a.created.Add(f.endCard)
		if !time.Now().Before(due) {
			a.state = "ended"
			return time.Time{}
		}
	}
	if f.cardAnswer == nil {
		return due
	}
	step, _ := a.content["step"].(float64)
	opt, after := f.cardAnswer(int(step))
	if opt == "" {
		return due
	}
	at := a.since.Add(after)
	if time.Now().Before(at) {
		if due.IsZero() || at.Before(due) {
			due = at
		}
		return due
	}
	a.content["answer"] = map[string]any{"option": opt, "by": "user", "at": time.Now().Unix()}
	count, _ := a.content["step_count"].(float64)
	if step == 0 || step >= count || f.noSteps {
		a.state = "ended"
	}
	return time.Time{}
}

func (f *fakeAPI) settleNotification(id int, n *fakeNotification) (bool, time.Time) {
	if n.action != "" {
		return true, time.Time{}
	}
	if f.notifAnswer == nil {
		return false, time.Time{}
	}
	action, text, after := f.notifAnswer(id, n.sent)
	if action == "" {
		return false, time.Time{}
	}
	at := n.at.Add(after)
	if time.Now().Before(at) {
		return false, at
	}
	n.action, n.text = action, text
	return true, time.Time{}
}

func (f *fakeAPI) activityJSON() map[string]any {
	content := map[string]any{}
	for k, v := range f.act.content {
		content[k] = v
	}
	if opts, ok := content["options"].([]any); ok && f.optionURL != "" {
		filled := make([]any, len(opts))
		for i, o := range opts {
			m := map[string]any{"url": f.optionURL}
			for k, v := range o.(map[string]any) {
				m[k] = v
			}
			filled[i] = m
		}
		content["options"] = filled
	}
	out := map[string]any{"slug": "x", "state": f.act.state, "content": content}
	if f.act.state == "ongoing" && f.started >= 0 {
		out["devices_targeted"] = 1
		out["devices_started"] = f.started
	}
	return out
}

// refuseTarget answers a create with an empty target the way the server
// does for the key: an invalid target from an organization's, one a personal
// key cannot have from any other.
func (f *fakeAPI) refuseTarget() (int, any) {
	if f.org {
		return problem(422, "org.target_invalid", "target must name at least one group, tag or member")
	}
	return problem(422, codeTargetUnavailable, "target is for organization keys")
}

// probe reports whether c is a key check: a create with a target.
func probe(c call) bool {
	return c.Method == "POST" && (c.Path == "/activities" || c.Path == "/notifications") && c.Body["target"] != nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// sent is every request that matches method and path prefix.
func (f *fakeAPI) sent(method, prefix string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// notices are the notifications sent without actions.
func (f *fakeAPI) notices() []map[string]any {
	var out []map[string]any
	for _, c := range f.sent("POST", "/notifications") {
		if c.Path == "/notifications" && c.Body["actions"] == nil {
			out = append(out, c.Body)
		}
	}
	return out
}

// asked are the notifications sent with actions.
func (f *fakeAPI) asked() []map[string]any {
	var out []map[string]any
	for _, c := range f.sent("POST", "/notifications") {
		if c.Path == "/notifications" && c.Body["actions"] != nil {
			out = append(out, c.Body)
		}
	}
	return out
}

// steps are the content of every PATCH that showed something on the card.
func (f *fakeAPI) steps() []map[string]any {
	var out []map[string]any
	for _, c := range f.sent("PATCH", "/activities/") {
		if content, ok := c.Body["content"].(map[string]any); ok {
			out = append(out, content)
		}
	}
	return out
}

// ended reports whether the agent ended the card itself.
func (f *fakeAPI) ended() bool {
	for _, c := range f.sent("PATCH", "/activities/") {
		if c.Body["state"] == "ended" {
			return true
		}
	}
	return false
}

func (f *fakeAPI) retracted() []string {
	var out []string
	for _, c := range f.sent("POST", "/notifications/retract") {
		out = append(out, fmt.Sprint(c.Body["collapse_id"]))
	}
	return out
}

const testToken = "hlk_secret_test_key"

// newRunner backs a Runner with the CLI's API client talking through rt.
func newRunner(rt http.RoundTripper, opts Options) (*Runner, *bytes.Buffer) {
	c := &api.Client{BaseURL: "https://api.pushward.test", Token: testToken, HTTP: &http.Client{Transport: rt}}
	var logs bytes.Buffer
	call := func(ctx context.Context, op string, params map[string]string, query url.Values, body map[string]any) ([]byte, error) {
		var data []byte
		if body != nil {
			var err error
			if data, err = json.Marshal(body); err != nil {
				return nil, err
			}
		}
		resp, err := c.Call(ctx, op, params, query, data)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	return &Runner{Call: call, Log: log.New(&logs, "", 0), Opts: opts}, &logs
}

// away is the options of a user away from the desk.
func away() Options {
	o := DefaultOptions()
	o.Desk = "away"
	return o
}

var errDown = errors.New("connection refused")

// downTransport fails every request at the network.
type downTransport struct{}

func (downTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errDown }
