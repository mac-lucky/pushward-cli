package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMute = `{"id":"m1","target":"*","applies_to":"both","mode":"silent","until":"2026-09-29T13:30:00Z","created_at":"2026-09-29T12:00:00Z"}`

// useLocaltime points the /etc/localtime lookup at path for one test.
func useLocaltime(t *testing.T, path string) {
	old := localtime
	localtime = path
	t.Cleanup(func() { localtime = old })
}

func TestMuteAddBody(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /app/mutes", 201, testMute)
	link := filepath.Join(t.TempDir(), "localtime")
	if err := os.Symlink("/usr/share/zoneinfo/America/New_York", link); err != nil {
		t.Fatal(err)
	}
	useLocaltime(t, link)
	noTZ := map[string]string{"TZ": ""}

	for _, tc := range []struct {
		argv []string
		env  map[string]string
		body string
	}{
		{[]string{"mute", "add", "backup"}, nil, `{"applies_to":"both","target":"backup"}`},
		// testNow is 12:00 UTC.
		{[]string{"mutes", "add", "*", "--for", "90m", "--label", "Deploy"}, nil,
			`{"applies_to":"both","label":"Deploy","target":"*","until":"2026-09-29T13:30:00Z"}`},
		{[]string{"mute", "add", "backup", "--applies-to", "notifications", "--mode", "off", "--until", "2026-12-01T08:00:00+01:00"}, nil,
			`{"applies_to":"notifications","mode":"off","target":"backup","until":"2026-12-01T08:00:00+01:00"}`},
		// An end before the start wraps past midnight; the server takes it as is.
		{[]string{"mute", "add", "*", "--schedule", "22:00-07:00", "--tz", "Europe/Warsaw"}, nil,
			`{"applies_to":"both","schedule":{"end_minute":420,"start_minute":1320,"time_zone":"Europe/Warsaw"},"target":"*"}`},
		{[]string{"mute", "add", "*", "--schedule", "9:30-17:00"}, map[string]string{"TZ": "Asia/Tokyo"},
			`{"applies_to":"both","schedule":{"end_minute":1020,"start_minute":570,"time_zone":"Asia/Tokyo"},"target":"*"}`},
		{[]string{"mute", "add", "*", "--schedule", "00:00-23:59"}, map[string]string{"TZ": ":/usr/share/zoneinfo/Europe/Lisbon"},
			`{"applies_to":"both","schedule":{"end_minute":1439,"start_minute":0,"time_zone":"Europe/Lisbon"},"target":"*"}`},
		{[]string{"mute", "add", "*", "--schedule", "23:00-06:00"}, noTZ,
			`{"applies_to":"both","schedule":{"end_minute":360,"start_minute":1380,"time_zone":"America/New_York"},"target":"*"}`},
		{[]string{"mute", "add", "deploy", "--applies-to", "activities", "--for", "2h", "--schedule", "08:00-18:00", "--tz", "UTC"}, nil,
			`{"applies_to":"activities","schedule":{"end_minute":1080,"start_minute":480,"time_zone":"UTC"},"target":"deploy","until":"2026-09-29T14:00:00Z"}`},
		{[]string{"mute", "add", "nas", "-d", `{"applies_to":"notifications"}`}, nil, `{"applies_to":"notifications","target":"nas"}`},
	} {
		f.calls = nil
		r := runCLI(t, srv, tc.env, "", tc.argv...)
		if r.code != 0 {
			t.Errorf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
			continue
		}
		if len(f.calls) != 1 {
			t.Errorf("%v: %d calls, want 1", tc.argv, len(f.calls))
			continue
		}
		if got := mustJSON(t, f.calls[0].Body); got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
	}

	r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "mute", "add", "*", "--for", "90m")
	if want := "muted * until " + shortTime("2026-09-29T13:30:00Z") + " (m1)\n"; r.stdout != want {
		t.Errorf("add on a terminal printed %q, want %q", r.stdout, want)
	}
}

func TestMuteAddUsageErrors(t *testing.T) {
	f, srv := newFake(t)
	useLocaltime(t, filepath.Join(t.TempDir(), "missing"))
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"mute", "add", "x", "--for", "1h", "--until", "2026-12-01T08:00:00Z"}, "--for and --until are mutually exclusive"},
		{[]string{"mute", "add", "x", "--for", "0"}, "--for: must be at least 1s"},
		{[]string{"mute", "add", "x", "--for", "soon"}, "--for:"},
		{[]string{"mute", "add", "x", "--until", "tomorrow"}, "--until: want RFC 3339"},
		{[]string{"mute", "add", "x", "--tz", "Europe/Warsaw"}, "--tz only applies with --schedule"},
		{[]string{"mute", "add", "x", "--schedule", "22:00-22:00", "--tz", "UTC"}, "starts and ends at the same time"},
		{[]string{"mute", "add", "x", "--schedule", "22:00", "--tz", "UTC"}, "want a daily window like 22:00-07:00"},
		{[]string{"mute", "add", "x", "--schedule", "25:00-07:00", "--tz", "UTC"}, "want a daily window"},
		{[]string{"mute", "add", "x", "--schedule", "22:00-07:00"}, "cannot tell this machine's time zone; pass --tz"},
		{[]string{"mute", "add"}, "needs <target>"},
		{[]string{"mute", "add", "a", "b"}, "needs <target>"},
		{[]string{"mute", "remove"}, "needs <mute-id>"},
		{[]string{"mute", "list", "x"}, "takes no arguments"},
	} {
		r := runCLI(t, srv, map[string]string{"TZ": ""}, "", tc.argv...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.argv, r.code, r.stderr, ExitUsage, tc.want)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("usage errors made %d calls: %+v", len(f.calls), f.calls)
	}
}

func TestMuteList(t *testing.T) {
	f, srv := newFake(t)
	body := `{"mutes":[` + testMute + `,{"id":"m2","target":"backup","label":"Nightly run","applies_to":"notifications","mode":"off",` +
		`"schedule":{"start_minute":1320,"end_minute":420,"time_zone":"Europe/Warsaw"},"created_at":"2026-09-29T12:00:00Z"}]}`
	f.reply("GET /app/mutes", 200, body)

	r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "mute", "list")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 {
		t.Fatalf("list on a terminal: exit %d, printed %q", r.code, r.stdout)
	}
	for i, want := range [][]string{
		{"ID", "TARGET", "APPLIES TO", "MODE", "UNTIL", "SCHEDULE", "LABEL"},
		{"m1", "*", "both", "silent", shortTime("2026-09-29T13:30:00Z")},
		{"m2", "backup", "notifications", "off", "22:00-07:00 Europe/Warsaw", "Nightly run"},
	} {
		for _, cell := range want {
			if !strings.Contains(lines[i], cell) {
				t.Errorf("row %d %q lacks %q", i, lines[i], cell)
			}
		}
	}

	r = runCLI(t, srv, nil, "", "mute", "list", "--json")
	if strings.TrimSpace(r.stdout) != body {
		t.Errorf("--json printed %q, want the response", r.stdout)
	}

	f.reply("GET /app/mutes", 200, `{"mutes":null}`)
	r = runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "mute", "list")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 {
		t.Errorf("an empty list printed %q (exit %d), want the header alone", r.stdout, r.code)
	}

	f.reply("GET /app/mutes", 403, `{"status":403,"code":"integration_key.permission_denied","detail":"insufficient permissions: requires account:read"}`)
	r = runCLI(t, srv, nil, "", "mute", "list")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "requires account:read") || !strings.Contains(r.stderr, "hint: this needs a personal key with the account level at read or above. Every key starts without it: raise it in the app (Settings > Manage Keys), or from a key that has it with pushward key update <key-id> --account read;") {
		t.Errorf("403: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestMuteAddErrors(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /app/mutes", 422, `{"status":422,"code":"mute.limit_exceeded","detail":"an account can hold at most 100 mutes"}`)
	r := runCLI(t, srv, nil, "", "mute", "add", "backup")
	if r.code != ExitError || !strings.Contains(r.stderr, "422 mute.limit_exceeded: an account can hold at most 100 mutes") || strings.Contains(r.stderr, "hint:") {
		t.Errorf("422: exit %d, stderr %q", r.code, r.stderr)
	}
	f.reply("POST /app/mutes", 403, `{"status":403,"code":"integration_key.permission_denied","detail":"insufficient permissions: requires account:manage"}`)
	r = runCLI(t, srv, nil, "", "mute", "add", "backup")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "--account manage;") {
		t.Errorf("403: exit %d, stderr %q", r.code, r.stderr)
	}
	// Only a key below the account level gets the hint; another refusal keeps its own message.
	f.reply("POST /app/mutes", 403, `{"status":403,"code":"subscription.required","detail":"needs a subscription"}`)
	r = runCLI(t, srv, nil, "", "mute", "add", "backup")
	if r.code != ExitAuth || strings.Contains(r.stderr, "hint:") {
		t.Errorf("403 subscription.required: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestMuteRemove(t *testing.T) {
	f, srv := newFake(t)
	f.reply("DELETE /app/mutes/a%2Fb", 204, "")
	r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "mute", "remove", "a/b")
	if r.code != 0 || r.stdout != "removed mute a/b\n" {
		t.Errorf("remove: exit %d, printed %q (%s)", r.code, r.stdout, r.stderr)
	}
	if len(f.calls) != 1 || f.calls[0].Method != "DELETE" || f.calls[0].Path != "/app/mutes/a%2Fb" {
		t.Errorf("remove sent %+v, want one DELETE /app/mutes/a%%2Fb", f.calls)
	}

	f.reply("DELETE /app/mutes/m9", 404, `{"status":404,"code":"mute.not_found","detail":"mute not found"}`)
	r = runCLI(t, srv, nil, "", "mute", "remove", "m9")
	if r.code != ExitNotFound || !strings.Contains(r.stderr, "404 mute.not_found: mute not found\nhint: pushward mute list shows the ids") {
		t.Errorf("404: exit %d, stderr %q", r.code, r.stderr)
	}

	f.reply("DELETE /app/mutes/m1", 403, `{"status":403,"code":"integration_key.permission_denied","detail":"insufficient permissions: requires account:manage"}`)
	r = runCLI(t, srv, nil, "", "mute", "remove", "m1")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "requires account:manage\nhint: this needs a personal key with the account level at manage or above.") {
		t.Errorf("403: exit %d, stderr %q", r.code, r.stderr)
	}
}
