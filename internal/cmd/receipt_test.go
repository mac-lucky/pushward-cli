package cmd

import (
	"strings"
	"testing"
)

const testReceipt = `{"id":9,"answerable":true,"receipt":{"notification_id":9,"status":"active","repeat_seconds":120,"expires_at":"2026-09-29T12:30:00Z","repeats_sent":0}}`

func TestNotifyAck(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, testReceipt)
	r := runCLI(t, srv, nil, "", "notify", "--title", "db-1 down", "--body", "b",
		"--ack-repeat", "2m", "--ack-expire", "30m", "--ack-title", "On it",
		"--tag", "db-1", "--tag", "a,b", "--callback-url", "https://hooks.example.com/pw")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := `{"acknowledge":{"action_title":"On it","expire_seconds":1800,"repeat_seconds":120},"body":"b","callback_url":"https://hooks.example.com/pw","tags":["db-1","a,b"],"title":"db-1 down"}`
	if got := mustJSON(t, f.calls[0].Body); got != want {
		t.Errorf("body\n got  %s\n want %s", got, want)
	}

	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--ack")
	if got := mustJSON(t, f.calls[1].Body["acknowledge"]); r.code != 0 || got != `{}` {
		t.Errorf("--ack: exit %d acknowledge %s", r.code, got)
	}
	// acknowledge from --data counts as --ack.
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--data", `{"acknowledge":{"repeat_seconds":60}}`, "--tag", "x")
	if r.code != 0 || mustJSON(t, f.calls[2].Body["tags"]) != `["x"]` {
		t.Errorf("--data acknowledge: exit %d %s", r.code, r.stderr)
	}

	f.calls = nil
	for name, argv := range map[string][]string{
		"tag alone":      {"--tag", "x"},
		"callback alone": {"--callback-url", "https://hooks.example.com"},
		"bad repeat":     {"--ack-repeat", "soon"},
	} {
		r := runCLI(t, srv, nil, "", append([]string{"notify", "--title", "a", "--body", "b"}, argv...)...)
		if r.code != ExitUsage {
			t.Errorf("%s: exit %d %q", name, r.code, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("refused sends reached the API: %d calls", len(f.calls))
	}
}

func TestScheduleCreateAck(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/scheduled", 201, `{"id":3,"send_at":"2026-09-29T14:00:00Z"}`)
	r := runCLI(t, srv, nil, "", "schedule", "create", "--in", "2h", "--title", "a", "--body", "b", "--ack", "--tag", "nightly")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if b := f.calls[0].Body; mustJSON(t, b["acknowledge"]) != `{}` || mustJSON(t, b["tags"]) != `["nightly"]` {
		t.Errorf("body %v", b)
	}
}

func TestNotifyAckWait(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, testReceipt)
	polls := 0
	f.routes["GET /notifications/receipts/9"] = func(c call) (int, string) {
		polls++
		if c.Query != "wait=25" {
			t.Errorf("poll query %q", c.Query)
		}
		if polls < 3 {
			return 200, `{"notification_id":9,"status":"active"}`
		}
		return 200, `{"notification_id":9,"status":"acknowledged","action_id":"pw_ack"}`
	}
	// --ack is enough for --wait: the server adds the button.
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--ack", "--wait", "10m", "--jq", ".status")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "acknowledged" || polls != 3 {
		t.Fatalf("exit %d out %q err %q after %d polls", r.code, r.stdout, r.stderr, polls)
	}

	f.reply("GET /notifications/receipts/9", 200, `{"notification_id":9,"status":"expired"}`)
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--ack", "--wait", "10m")
	if r.code != ExitWaitTimeout || !strings.Contains(r.stderr, "expired before anyone acknowledged") {
		t.Errorf("expired: exit %d %q", r.code, r.stderr)
	}
}

func TestReceiptGet(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /notifications/receipts/42", 200, `{"notification_id":42,"status":"active"}`)
	for _, argv := range [][]string{{"receipt", "42"}, {"receipt", "get", "42"}, {"receipts", "get", "42"}} {
		r := runCLI(t, srv, nil, "", argv...)
		if r.code != 0 || !strings.Contains(r.stdout, `"status":"active"`) {
			t.Errorf("%v: exit %d %q %q", argv, r.code, r.stdout, r.stderr)
		}
	}
	if len(f.calls) != 3 || f.calls[0].Query != "" {
		t.Errorf("calls %+v", f.calls)
	}
	r := runCLI(t, srv, nil, "", "receipt", "42", "--wait", "1m")
	if r.code != ExitWaitTimeout {
		t.Errorf("--wait on an active receipt: exit %d", r.code)
	}
	// A finished receipt is a result, not an error, without --wait.
	f.reply("GET /notifications/receipts/42", 200, `{"notification_id":42,"status":"canceled","cancel_reason":"tag"}`)
	if r := runCLI(t, srv, nil, "", "receipt", "get", "42"); r.code != 0 {
		t.Errorf("canceled: exit %d", r.code)
	}
	for _, argv := range [][]string{{"receipt", "nope"}, {"receipt", "get", "x"}} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d", argv, r.code)
		}
	}
}

func TestReceiptCancel(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/receipts/42/cancel", 200, `{"notification_id":42,"status":"canceled","cancel_reason":"api"}`)
	f.reply("POST /notifications/receipts/cancel", 200, `{"canceled":2}`)
	if r := runCLI(t, srv, nil, "", "receipt", "cancel", "42"); r.code != 0 || f.calls[0].Body != nil {
		t.Errorf("cancel 42: exit %d %q body %v", r.code, r.stderr, f.calls[0].Body)
	}
	r := runCLI(t, srv, nil, "", "receipt", "cancel", "--tag", "db-1", "--jq", ".canceled")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "2" || mustJSON(t, f.calls[1].Body) != `{"tag":"db-1"}` {
		t.Errorf("cancel --tag: exit %d %q %v", r.code, r.stdout, f.calls[1].Body)
	}
	for _, argv := range [][]string{{}, {"42", "--tag", "x"}, {"x"}, {"1", "2"}} {
		if r := runCLI(t, srv, nil, "", append([]string{"receipt", "cancel"}, argv...)...); r.code != ExitUsage {
			t.Errorf("cancel %v: exit %d", argv, r.code)
		}
	}
	if len(f.calls) != 2 {
		t.Errorf("%d calls", len(f.calls))
	}
}

func TestReceiptSecret(t *testing.T) {
	_, srv := newFake(t)
	// The first case of internal/callback/testdata/callback-vectors-v1.json.
	const key, want = "hlk_0123456789abcdef0123456789abcdef", "whsec_I1p3Yj83UaqkDUbjL1juMcCeRaMo2WAxaTS/hpRZYfA="
	t.Setenv("PUSHWARD_API_TOKEN", key)
	r := runCLI(t, srv, nil, "", "receipt", "secret", "--jq", ".secret")
	if r.code != 0 || strings.TrimSpace(r.stdout) != want {
		t.Errorf("configured key: exit %d %q %q", r.code, r.stdout, r.stderr)
	}
	t.Setenv("PUSHWARD_API_TOKEN", "")
	r = runCLI(t, srv, nil, "", "receipt", "secret", "--key", key, "--jq", ".secret")
	if r.code != 0 || strings.TrimSpace(r.stdout) != want {
		t.Errorf("--key: exit %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := runCLI(t, srv, nil, "", "receipt", "secret"); r.code != ExitAuth {
		t.Errorf("no key: exit %d", r.code)
	}
	if r := runCLI(t, srv, nil, "", "receipt", "secret", "--key", "hla_account"); r.code != ExitUsage {
		t.Errorf("hla_ key: exit %d", r.code)
	}
}

func TestDescribeReceipt(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"notification_id":1,"status":"active","repeats_sent":3,"repeat_seconds":60,"expires_at":"bad"}`,
			"notification 1: active, repeated 3 times, every 60s until bad\n"},
		{`{"notification_id":1,"status":"acknowledged","acknowledged_at":"x","acknowledged_by_device":"iPhone","action_id":"pw_ack","callback":{"status":"delivered","attempts":1}}`,
			"notification 1: acknowledged x by iPhone\ncallback delivered after 1 attempt\n"},
		{`{"notification_id":1,"status":"acknowledged","acknowledged_at":"x","acknowledged_by":"u1","action_id":"deploy"}`,
			"notification 1: acknowledged x by u1 with deploy\n"},
		{`{"notification_id":1,"status":"canceled","canceled_at":"x","cancel_reason":"superseded","callback":{"status":"failed","attempts":7,"last_status_code":500}}`,
			"notification 1: canceled x (superseded)\ncallback failed after 7 attempts (HTTP 500)\n"},
	} {
		if got := describeReceipt(decode([]byte(tc.body))); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.body, got, tc.want)
		}
	}
}
