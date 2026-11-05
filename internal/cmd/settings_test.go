package cmd

import (
	"strings"
	"testing"
)

const testRecipientID = "9b1c2d3e-4f50-4617-8a9b-0c1d2e3f4a5b"

func TestSettingsCommands(t *testing.T) {
	f, srv := newFake(t)
	// An org admin key in the environment must not leak into these.
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	for _, tc := range []struct {
		argv   []string
		routes []string // method, path and query of each request
		body   string   // the first request's body, "" for none
		status int
	}{
		{[]string{"device", "list"}, []string{"GET /devices"}, "", 200},
		{[]string{"device", "get", "d1"}, []string{"GET /devices/d1"}, "", 200},
		{[]string{"device", "rename", "d1", "Work phone"}, []string{"PATCH /devices/d1"}, `{"name":"Work phone"}`, 204},
		{[]string{"device", "delete", "d1"}, []string{"DELETE /devices/d1"}, "", 204},
		{[]string{"device", "follows", "d1"}, []string{"GET /devices/d1/subscriptions"}, "", 200},
		{[]string{"device", "follows", "d1", "--activity", "deploy"}, []string{"GET /devices/d1/subscriptions/deploy"}, "", 200},
		{[]string{"device", "follow", "d1", "deploy", "backup"}, []string{"POST /devices/d1/subscriptions/deploy", "POST /devices/d1/subscriptions/backup"}, "", 204},
		{[]string{"device", "unfollow", "d1", "deploy"}, []string{"DELETE /devices/d1/subscriptions/deploy"}, "", 204},
		{[]string{"share", "list"}, []string{"GET /permissions"}, "", 200},
		{[]string{"share", "list", "--direction", "received"}, []string{"GET /permissions?direction=received"}, "", 200},
		{[]string{"share", "list", "--activity", "deploy"}, []string{"GET /activities/deploy/permissions"}, "", 200},
		{[]string{"share", "grant", "deploy", "--role", "viewer", "--expires-in", "1h", "--max-uses", "3"}, []string{"POST /activities/deploy/permissions"},
			`{"grantee":{"expires_in":3600,"max_uses":3,"type":"share_link"},"role":"viewer"}`, 201},
		{[]string{"share", "grant", "deploy", "--user", "u2", "--role", "editor"}, []string{"POST /activities/deploy/permissions"},
			`{"grantee":{"type":"user","user_id":"u2"},"role":"editor"}`, 201},
		{[]string{"share", "grant-pattern", "ci-*", "--max-uses", "2"}, []string{"POST /permissions/patterns"}, `{"max_uses":2,"slug_pattern":"ci-*"}`, 201},
		{[]string{"share", "update", "perm_ps_7", "--role", "editor"}, []string{"PATCH /permissions/perm_ps_7"}, `{"role":"editor"}`, 200},
		{[]string{"share", "revoke", "perm_ps_7"}, []string{"DELETE /permissions/perm_ps_7"}, "", 204},
		{[]string{"me", "update", "--nickname", "Maciej"}, []string{"PATCH /auth/me"}, `{"nickname":"Maciej"}`, 204},
		{[]string{"me", "onboarding"}, []string{"GET /app/onboarding"}, "", 200},
		{[]string{"me", "onboarding", "get"}, []string{"GET /app/onboarding"}, "", 200},
		{[]string{"me", "onboarding", "update", "--nudges", "false", "--use-case", "github"}, []string{"PATCH /app/onboarding"}, `{"nudges_enabled":false,"use_case":"github"}`, 200},
		{[]string{"email", "recipient", "list"}, []string{"GET /emails/recipients"}, "", 200},
		{[]string{"email", "recipient", "add", "ops@example.com", "--label", "Ops"}, []string{"POST /emails/recipients"}, `{"address":"ops@example.com","label":"Ops"}`, 201},
		{[]string{"email", "recipient", "remove", testRecipientID}, []string{"DELETE /emails/recipients/" + testRecipientID}, "", 204},
		{[]string{"email", "recipient", "resend", testRecipientID}, []string{"POST /emails/recipients/" + testRecipientID + "/resend"}, `{}`, 200},
	} {
		resp := `{"id":"x","items":[]}`
		if tc.status == 204 {
			resp = ""
		}
		for _, r := range tc.routes {
			route, _, _ := strings.Cut(r, "?")
			f.reply(route, tc.status, resp)
		}
		f.calls = nil
		r := runCLI(t, srv, nil, "", tc.argv...)
		if r.code != 0 {
			t.Errorf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
			continue
		}
		var sent []string
		for _, c := range f.calls {
			s := c.Method + " " + c.Path
			if c.Query != "" {
				s += "?" + c.Query
			}
			sent = append(sent, s)
			if c.Auth != "Bearer hlk_test" {
				t.Errorf("%v: %s sent %q, want the integration key", tc.argv, s, c.Auth)
			}
		}
		if strings.Join(sent, ", ") != strings.Join(tc.routes, ", ") {
			t.Errorf("%v: sent %v, want %v", tc.argv, sent, tc.routes)
			continue
		}
		got := ""
		if f.calls[0].Body != nil {
			got = mustJSON(t, f.calls[0].Body)
		}
		if got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
		if strings.TrimSpace(r.stdout) != resp {
			t.Errorf("%v: piped output %q, want the raw response", tc.argv, r.stdout)
		}
	}
}

func TestSettingsUsageErrors(t *testing.T) {
	f, srv := newFake(t)
	for _, argv := range [][]string{
		{"device", "follow", "d1"},
		{"device", "follow", "d1", "bad slug"},
		{"device", "follows", "d1", "--activity", "bad slug"},
		{"device", "rename", "d1"},
		{"share", "list", "--activity", "deploy", "--direction", "received"},
		{"share", "grant", "deploy", "--user", "u2", "--max-uses", "2"},
		{"share", "grant", "deploy", "--expires-in", "soon"},
		{"me", "update"},
		{"me", "extra"},
		{"me", "onboarding", "update", "--nudges", "maybe"},
		{"email", "recipient", "add"},
	} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", argv, r.code, ExitUsage, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("usage errors made %d calls: %+v", len(f.calls), f.calls)
	}
}

func TestSettingsHumanOutput(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /devices", 200, `{"items":[{"device_id":"d1","name":"iPhone","platform":"ios","device_model":"iPhone17,1","app_version":"1.20.0","last_active_at":"2026-01-02T03:04:05Z","is_current":true}],"limit":5}`)
	f.reply("GET /devices/d1/subscriptions", 200, `{"items":["deploy","backup"],"activity_ids":["a1","a2"]}`)
	f.reply("GET /devices/d1/subscriptions/deploy", 200, `{"is_subscribed":true}`)
	f.reply("POST /devices/d1/subscriptions/deploy", 204, "")
	f.reply("GET /permissions", 200, `{"items":[
		{"id":"perm_us_1","role":"viewer","activity_slug":"deploy","grantee":{"type":"user","user":{"id":"u2","nickname":"Ola"}},"owner_id":"u1","created_at":"2026-01-02T03:04:05Z"},
		{"id":"perm_pc_2","role":"viewer","slug_pattern":"ci-*","grantee":{"type":"pattern_link","pattern_link":{"use_count":1,"max_uses":3,"expires_at":"2030-01-01T00:00:00Z"}},"owner_id":"u1","created_at":"2026-01-02T03:04:05Z"}]}`)
	f.reply("POST /activities/deploy/permissions", 201, `{"id":"perm_sc_3","role":"viewer","activity_slug":"deploy","grantee":{"type":"share_link","share_link":{"share_url":"https://pushward.app/s/abc","expires_at":"2030-01-01T00:00:00Z","use_count":0}}}`)
	f.reply("GET /emails/recipients", 200, `{"items":[{"id":"`+testRecipientID+`","address":"ops@example.com","status":"pending","created_at":"2026-01-02T03:04:05Z"}]}`)
	f.reply("POST /emails/recipients", 201, `{"id":"`+testRecipientID+`","address":"ops@example.com","status":"pending"}`)
	f.reply("PATCH /auth/me", 204, "")

	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"device", "list"}, []string{"DEVICE ID", "d1", "iPhone17,1", "1.20.0", "yes"}},
		{[]string{"device", "follows", "d1"}, []string{"deploy\nbackup\n"}},
		{[]string{"device", "follows", "d1", "--activity", "deploy"}, []string{"d1 follows deploy: true"}},
		{[]string{"device", "follow", "d1", "deploy"}, []string{"d1 follows deploy"}},
		{[]string{"share", "list"}, []string{"perm_us_1", "user Ola", "ci-*", "link 1/3 uses"}},
		{[]string{"share", "grant", "deploy"}, []string{"shared deploy as viewer with link 0/any uses (perm_sc_3)", "https://pushward.app/s/abc"}},
		{[]string{"email", "recipient", "list"}, []string{"ADDRESS", "ops@example.com", "pending"}},
		{[]string{"email", "recipient", "add", "ops@example.com"}, []string{"added ops@example.com (pending)"}},
		{[]string{"me", "update", "--nickname", "Maciej"}, []string{"updated the profile"}},
	} {
		out := runTTY(t, srv, tc.argv...)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q:\n%s", tc.argv, w, out)
			}
		}
	}
	if out := runTTY(t, srv, "device", "list", "--json"); !strings.Contains(out, `"device_id": "d1"`) {
		t.Errorf("--json printed %q", out)
	}
}

func TestKeyAccountLevels(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /integrations/keys", 201, `{"id":"`+testKeyID+`","name":"n","key":"hlk_new"}`)
	f.reply("PATCH /integrations/keys/"+testKeyID, 200, `{"id":"`+testKeyID+`"}`)
	f.reply("GET /integrations/keys", 200, `{"items":[{"id":"`+testKeyID+`","name":"k","permissions":{"activities":"none","notifications":"none","widgets":"none","emails":"none","devices":"read","sharing":"manage","account":"none"}}]}`)
	for _, tc := range []struct {
		argv []string
		body string
	}{
		{[]string{"key", "create", "n", "--devices", "read", "--sharing", "manage", "--account", "none"}, `{"name":"n","permissions":{"account":"none","activities":"update","devices":"read","sharing":"manage"}}`},
		{[]string{"key", "update", testKeyID, "--account", "manage"}, `{"permissions":{"account":"manage"}}`},
	} {
		f.calls = nil
		if r := runCLI(t, srv, nil, "", append(tc.argv, "--jq", ".id")...); r.code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
		}
		if got := mustJSON(t, f.calls[0].Body); got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
	}
	if r := runCLI(t, srv, nil, "", "key", "create", "n", "--devices", "write"); r.code != ExitUsage {
		t.Errorf("--devices write: exit %d", r.code)
	}
	lines := strings.Split(runTTY(t, srv, "key", "list"), "\n")
	if !strings.Contains(lines[1], "devices:read sharing:manage") || strings.Contains(lines[1], "account") {
		t.Errorf("key list row: %q", lines[1])
	}
}
