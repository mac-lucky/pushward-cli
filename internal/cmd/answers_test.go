package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
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
	if r := runCLI(t, srv, nil, "", "notification", "answers", "--limit", "101"); r.code != ExitUsage {
		t.Errorf("--limit 101: exit %d", r.code)
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
	// The full wait slots back off 5s, the empty page that came back at
	// once 2s.
	if len(r.sleeps) != 2 || r.sleeps[0] != 5*time.Second || r.sleeps[1] != 2*time.Second {
		t.Errorf("sleeps %v", r.sleeps)
	}

	if r := runCLI(t, srv, nil, "", "notification", "answers", "--follow", "--wait", "1m"); r.code != ExitUsage {
		t.Errorf("--follow with --wait: exit %d", r.code)
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
