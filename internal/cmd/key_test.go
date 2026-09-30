package cmd

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testKeyID = "0b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11"

func TestKeyCreate(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /integrations/keys", 201, `{"id":"`+testKeyID+`","name":"backup","key":"hlk_new"}`)

	r := runCLI(t, srv, nil, "", "key", "create", "backup", "--scope", "activity:manage", "--activity-slugs", "backup-*, nas", "--notifications", "--widgets=false")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got, want := mustJSON(t, f.calls[0].Body), `{"activity_slugs":["backup-*","nas"],"name":"backup","notifications":true,"scope":"activity:manage","widgets":false}`; got != want {
		t.Errorf("body %s, want %s", got, want)
	}
	if !strings.Contains(r.stdout, `"key":"hlk_new"`) {
		t.Errorf("stdout %q, want the raw response", r.stdout)
	}

	// Unset capabilities stay out of the body, so the server defaults apply.
	r = runCLI(t, srv, nil, "", "key", "create", "plain", "--jq", ".key")
	if got := mustJSON(t, f.calls[1].Body); got != `{"name":"plain"}` {
		t.Errorf("body %s", got)
	}
	if r.stdout != "hlk_new\n" {
		t.Errorf("--jq .key printed %q", r.stdout)
	}

	// -q would throw away the only copy of the secret.
	for _, argv := range [][]string{{"key", "create", "x", "-q"}, {"key", "roll", testKeyID, "-q"}} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage || !strings.Contains(r.stderr, "--jq .key") {
			t.Errorf("%v: exit %d, %s", argv, r.code, r.stderr)
		}
	}
	if len(f.calls) != 2 {
		t.Errorf("%d calls, want 2", len(f.calls))
	}
}

func TestKeyUpdate(t *testing.T) {
	f, srv := newFake(t)
	f.reply("PATCH /integrations/keys/"+testKeyID, 200, `{"id":"`+testKeyID+`"}`)

	for _, tc := range []struct {
		argv []string
		body string
	}{
		{[]string{"--all-activities"}, `{"activity_slugs":[]}`},
		{[]string{"--notifications=false"}, `{"notifications":false}`},
		{[]string{"--scope", "activity:update", "--activity-slugs", "ci-*", "--emails"}, `{"activity_slugs":["ci-*"],"emails":true,"scope":"activity:update"}`},
		{[]string{"-F", "widgets=false"}, `{"widgets":false}`},
	} {
		f.calls = nil
		r := runCLI(t, srv, nil, "", append([]string{"key", "update", testKeyID}, tc.argv...)...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
		}
		if got := mustJSON(t, f.calls[0].Body); got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
	}

	f.calls = nil
	for _, argv := range [][]string{
		{},
		{"--data", "{}"},
		{"--all-activities", "--activity-slugs", "a"},
	} {
		if r := runCLI(t, srv, nil, "", append([]string{"key", "update", testKeyID}, argv...)...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", argv, r.code, ExitUsage)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("usage errors made %d calls", len(f.calls))
	}
}

func TestKeyRevokeAndRoll(t *testing.T) {
	f, srv := newFake(t)
	f.reply("DELETE /integrations/keys/"+testKeyID, 204, "")
	f.reply("POST /integrations/keys/"+testKeyID+"/roll", 200, `{"id":"`+testKeyID+`","key":"hlk_rolled"}`)

	r := runCLI(t, srv, nil, "", "keys", "revoke", testKeyID)
	if r.code != 0 || r.app.Status != "revoked" {
		t.Fatalf("revoke: exit %d, status %q: %s", r.code, r.app.Status, r.stderr)
	}
	r = runCLI(t, srv, nil, "", "key", "roll", testKeyID)
	if r.code != 0 || !strings.Contains(r.stdout, "hlk_rolled") {
		t.Fatalf("roll: exit %d, stdout %q: %s", r.code, r.stdout, r.stderr)
	}
	if f.calls[1].Body != nil {
		t.Errorf("roll sent a body: %v", f.calls[1].Body)
	}
}

func TestKeyErrors(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /integrations/keys", 403, `{"status":403,"code":"integration_key.default_key_required","detail":"only the default key can manage keys"}`)

	r := runCLI(t, srv, nil, "", "key", "list")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "integration_key.default_key_required") {
		t.Errorf("403: exit %d, stderr %q", r.code, r.stderr)
	}
	f.calls = nil
	for _, id := range []string{"123", "../x", testKeyID + "x"} {
		if r := runCLI(t, srv, nil, "", "key", "revoke", id); r.code != ExitUsage {
			t.Errorf("revoke %q: exit %d, want %d", id, r.code, ExitUsage)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("bad ids made %d calls", len(f.calls))
	}
}

// runTTY runs the CLI as if stdout were a terminal, for the human output.
func runTTY(t *testing.T, srv *httptest.Server, argv ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, StdoutTTY: true, HTTP: srv.Client(), Now: time.Now, Getenv: os.Getenv}
	if code := Execute(a, argv); code != 0 {
		t.Fatalf("%v: exit %d: %s", argv, code, errb.String())
	}
	return out.String()
}

func TestKeyHumanOutput(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /integrations/keys", 200, `{"items":[
		{"id":"`+testKeyID+`","name":"Default","permissions":{"activities":"manage","notifications":"schedule","widgets":"write","emails":"send"},"scope":"activity:manage","activity_slugs":null,"is_default":true},
		{"id":"1b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11","name":"backup","permissions":{"activities":"none","notifications":"send","widgets":"read","emails":"none"},"activity_slugs":["backup-*","nas"],"widget_slugs":["nas-*"],"expires_at":"2030-01-01T00:00:00Z","is_default":false},
		{"id":"2b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11","name":"legacy","scope":"activity:update","notifications":true,"is_default":false}]}`)
	f.reply("POST /integrations/keys", 201, `{"id":"`+testKeyID+`","name":"backup","key":"hlk_new"}`)

	lines := strings.Split(runTTY(t, srv, "key", "list"), "\n")
	if !strings.Contains(lines[1], "activities:manage notifications:schedule widgets:write emails:send") || !strings.Contains(lines[1], "yes") {
		t.Errorf("default key row: %q", lines[1])
	}
	if !strings.Contains(lines[2], "notifications:send widgets:read") || strings.Contains(lines[2], "activities:") ||
		!strings.Contains(lines[2], "backup-*,nas") || !strings.Contains(lines[2], "nas-*") || strings.Contains(lines[2], "yes") {
		t.Errorf("minted key row: %q", lines[2])
	}
	// A server without levels sends only the legacy fields.
	if !strings.Contains(lines[3], "activity:update notifications") {
		t.Errorf("legacy key row: %q", lines[3])
	}

	out := runTTY(t, srv, "key", "create", "backup")
	if !strings.HasPrefix(out, "created key backup ("+testKeyID+")\nhlk_new\n") {
		t.Errorf("create printed %q", out)
	}
}

func TestKeyPermissionLevels(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /integrations/keys", 201, `{"id":"`+testKeyID+`","name":"n","key":"hlk_new"}`)
	f.reply("PATCH /integrations/keys/"+testKeyID, 200, `{"id":"`+testKeyID+`"}`)
	now := time.Now()

	for _, tc := range []struct {
		argv []string
		body string
	}{
		// A level anywhere moves everything into permissions, legacy flags included.
		// On create activities stay at update unless --activities says otherwise,
		// so --x and --x=<top> build the same key.
		{[]string{"key", "create", "n", "--notifications=send"}, `{"name":"n","permissions":{"activities":"update","notifications":"send"}}`},
		{[]string{"key", "create", "n", "--activities", "none", "--notifications=send"}, `{"name":"n","permissions":{"activities":"none","notifications":"send"}}`},
		{[]string{"key", "create", "n", "--emails=send"}, `{"name":"n","permissions":{"activities":"update","emails":"send"}}`},
		{[]string{"key", "create", "n", "--emails=1"}, `{"emails":true,"name":"n"}`},
		{[]string{"key", "create", "n", "--emails=True", "--widgets=0"}, `{"emails":true,"name":"n","widgets":false}`},
		{[]string{"key", "create", "n", "--activities", "read", "--widgets", "--emails=false"}, `{"name":"n","permissions":{"activities":"read","emails":"none","widgets":"write"}}`},
		{[]string{"key", "create", "n", "--scope", "activity:none", "--notifications=send"}, `{"name":"n","permissions":{"activities":"none","notifications":"send"}}`},
		// true/false alone keeps the legacy body.
		{[]string{"key", "create", "n", "--scope", "activity:read", "--widgets"}, `{"name":"n","scope":"activity:read","widgets":true}`},
		{[]string{"key", "create", "n", "--widget-slugs", "cpu,disk-*", "--expires", "2030-01-02T03:04:05Z"}, `{"expires_at":"2030-01-02T03:04:05Z","name":"n","widget_slugs":["cpu","disk-*"]}`},
		{[]string{"key", "update", testKeyID, "--widgets=read"}, `{"permissions":{"widgets":"read"}}`},
		{[]string{"key", "update", testKeyID, "--scope", "activity:manage", "--widgets=read"}, `{"permissions":{"activities":"manage","widgets":"read"}}`},
		{[]string{"key", "update", testKeyID, "--all-widgets", "--no-expiry"}, `{"expires_at":null,"widget_slugs":[]}`},
	} {
		f.calls = nil
		r := runCLI(t, srv, nil, "", append(tc.argv, "--jq", ".id")...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
		}
		if got := mustJSON(t, f.calls[0].Body); got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
	}

	// --expires takes a duration from now.
	f.calls = nil
	if r := runCLI(t, srv, nil, "", "key", "create", "n", "--expires", "30d", "--jq", ".id"); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	exp, err := time.Parse(time.RFC3339, fmt.Sprint(f.calls[0].Body["expires_at"]))
	if err != nil || exp.Sub(now) < 29*24*time.Hour || exp.Sub(now) > 31*24*time.Hour {
		t.Errorf("--expires 30d sent %v (%v)", f.calls[0].Body["expires_at"], err)
	}

	f.calls = nil
	for _, argv := range [][]string{
		{"key", "create", "n", "--widgets=admin"},
		{"key", "create", "n", "--notifications", "send"},
		{"key", "update", testKeyID, "--widgets", "read"},
		{"key", "create", "n", "--activities", "true"},
		{"key", "create", "n", "--activities", "read", "--scope", "activity:read"},
		{"key", "update", testKeyID, "--all-widgets", "--widget-slugs", "a"},
		{"key", "update", testKeyID, "--no-expiry", "--expires", "1d"},
		{"key", "create", "n", "--expires", "whenever"},
		// A level word taken as the name, or a stray level anywhere.
		{"key", "create", "--widgets", "read"},
		{"key", "create", "--notifications", "send", "backup"},
		{"key", "update", testKeyID, "--scope", "manage", "--widgets=read"},
		{"key", "create", "n", "--scope", "admin"},
	} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", argv, r.code, ExitUsage, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("rejected flags made %d calls", len(f.calls))
	}
}

func TestKeyListAndStatusShowPermissions(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /integrations/keys", 200, `{"items":[{"id":"`+testKeyID+`","name":"k","permissions":{"activities":"read","notifications":"none","widgets":"none","emails":"none"},"expires_at":"2030-01-01T00:00:00Z"}]}`)
	f.reply("GET /auth/me", 200, `{"id":"u1","subscribed":true,"activity_count":2,"integration_key":{"id":"`+testKeyID+`","name":"ci","permissions":{"activities":"update","notifications":"send","widgets":"none","emails":"none"},"widget_slugs":["cpu-*"],"expires_at":"2030-01-01T00:00:00Z"}}`)

	lines := strings.Split(runTTY(t, srv, "key", "list"), "\n")
	if !strings.Contains(lines[0], "EXPIRES") || !strings.Contains(lines[1], "activities:read") || !strings.Contains(lines[1], "2030") {
		t.Errorf("key list: %q", lines[:2])
	}
	out := runTTY(t, srv, "me")
	for _, want := range []string{"key name     ci (" + testKeyID + ")", "permissions  activities:update notifications:send", "limited to   activities all, widgets cpu-*", "expires      2030"} {
		if !strings.Contains(out, want) {
			t.Errorf("me output lacks %q:\n%s", want, out)
		}
	}
}
