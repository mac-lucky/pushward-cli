package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-cli/internal/config"
)

const testOrgs = `{"items":[{"id":"o1","name":"Acme","slug":"acme","role":"admin","plan":"team","status":"active","member_count":3,"member_limit":10}]}`

// testOrgKey is long enough for auth status to mask it as hlo_new_...ey.
const testOrgKey = "hlo_new" + "_org_admin_key"

func TestOrgCommands(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	for _, tc := range []struct {
		argv   []string
		route  string // method and path the command must send after GET /orgs
		body   string // request body, "" for none
		status int
	}{
		{[]string{"org", "get"}, "GET /orgs/o1", "", 200},
		{[]string{"org", "update", "--name", "Acme Ltd"}, "PATCH /orgs/o1", `{"name":"Acme Ltd"}`, 200},
		{[]string{"org", "member", "get", "u1"}, "GET /orgs/o1/members/u1", "", 200},
		{[]string{"org", "member", "invite", "ops@example.com", "--role", "member", "--note", "Ops", "--expires-in", "2d"}, "POST /orgs/o1/invites",
			`{"email":"ops@example.com","expires_in_hours":48,"note":"Ops","role":"member"}`, 201},
		{[]string{"org", "member", "update", "u1", "--role", "viewer", "--email", "u1@example.com"}, "PATCH /orgs/o1/members/u1", `{"email":"u1@example.com","role":"viewer"}`, 200},
		{[]string{"org", "member", "update", "u1", "--clear-email"}, "PATCH /orgs/o1/members/u1", `{"email":null}`, 200},
		{[]string{"org", "member", "remove", "u1"}, "DELETE /orgs/o1/members/u1", "", 204},
		{[]string{"org", "invite", "get", "i1"}, "GET /orgs/o1/invites/i1", "", 200},
		{[]string{"org", "invite", "create", "--role", "viewer", "--max-uses", "5"}, "POST /orgs/o1/invites", `{"max_uses":5,"role":"viewer"}`, 201},
		{[]string{"org", "invite", "revoke", "i1"}, "DELETE /orgs/o1/invites/i1", "", 204},
		{[]string{"org", "key", "list"}, "GET /orgs/o1/keys", "", 200},
		{[]string{"org", "key", "get", testKeyID}, "GET /orgs/o1/keys/" + testKeyID, "", 200},
		{[]string{"org", "key", "create", "ci", "--activities", "manage", "--notifications=schedule", "--activity-slugs", "ci-*", "--target-group-ids", "g1,g2", "--target-tag-ids", "t1"},
			"POST /orgs/o1/keys", `{"activity_slugs":["ci-*"],"name":"ci","permissions":{"activities":"manage","notifications":"schedule"},"target":{"group_ids":["g1","g2"],"tag_ids":["t1"]}}`, 201},
		{[]string{"org", "key", "create", "plain"}, "POST /orgs/o1/keys", `{"name":"plain"}`, 201},
		{[]string{"org", "key", "update", testKeyID, "--name", "ci2", "--widgets", "write", "--all-activities", "--no-expiry"},
			"PATCH /orgs/o1/keys/" + testKeyID, `{"activity_slugs":[],"expires_at":null,"name":"ci2","permissions":{"widgets":"write"}}`, 200},
		{[]string{"org", "key", "roll", testKeyID}, "POST /orgs/o1/keys/" + testKeyID + "/roll", "", 200},
		{[]string{"org", "key", "revoke", testKeyID}, "DELETE /orgs/o1/keys/" + testKeyID, "", 204},
		{[]string{"org", "group", "list"}, "GET /orgs/o1/groups", "", 200},
		{[]string{"org", "group", "get", "g1"}, "GET /orgs/o1/groups/g1", "", 200},
		{[]string{"org", "group", "create", "oncall", "--description", "On call"}, "POST /orgs/o1/groups", `{"description":"On call","name":"oncall"}`, 201},
		{[]string{"org", "group", "update", "g1", "--name", "sre"}, "PATCH /orgs/o1/groups/g1", `{"name":"sre"}`, 200},
		{[]string{"org", "group", "delete", "g1"}, "DELETE /orgs/o1/groups/g1", "", 204},
		{[]string{"org", "group", "set-members", "g1", "u1", "u2"}, "PUT /orgs/o1/groups/g1/members", `{"user_ids":["u1","u2"]}`, 200},
		{[]string{"org", "group", "set-members", "g1"}, "PUT /orgs/o1/groups/g1/members", `{"user_ids":[]}`, 200},
		{[]string{"org", "tag", "list"}, "GET /orgs/o1/tags", "", 200},
		{[]string{"org", "tag", "get", "t1"}, "GET /orgs/o1/tags/t1", "", 200},
		{[]string{"org", "tag", "create", "wall"}, "POST /orgs/o1/tags", `{"name":"wall"}`, 201},
		{[]string{"org", "tag", "update", "t1", "--description", ""}, "PATCH /orgs/o1/tags/t1", `{"description":""}`, 200},
		{[]string{"org", "tag", "delete", "t1"}, "DELETE /orgs/o1/tags/t1", "", 204},
		{[]string{"org", "tag", "devices", "t1", "--add", "u1:d1", "--add", "u2:d2", "--remove", "u3:d:3"}, "POST /orgs/o1/tags/t1/devices",
			`{"add":[{"device_id":"d1","user_id":"u1"},{"device_id":"d2","user_id":"u2"}],"remove":[{"device_id":"d:3","user_id":"u3"}]}`, 200},
		{[]string{"org", "route", "list"}, "GET /orgs/o1/routes", "", 200},
		{[]string{"org", "route", "get", "r1"}, "GET /orgs/o1/routes/r1", "", 200},
		{[]string{"org", "route", "create", "deploy-*", "--applies-to", "both", "--allow-group-ids", "g1", "--note", "ops"}, "POST /orgs/o1/routes",
			`{"allow_group_ids":["g1"],"applies_to":"both","note":"ops","slug_pattern":"deploy-*"}`, 201},
		{[]string{"org", "route", "update", "r1", "--slug-pattern", "ci-*", "--allow-tag-ids", "t1,t2"}, "PATCH /orgs/o1/routes/r1",
			`{"allow_tag_ids":["t1","t2"],"slug_pattern":"ci-*"}`, 200},
		{[]string{"org", "route", "update", "r1", "--allow-tag-ids", "t1", "-F", "allow_group_ids=[]"}, "PATCH /orgs/o1/routes/r1",
			`{"allow_group_ids":[],"allow_tag_ids":["t1"]}`, 200},
		{[]string{"org", "route", "delete", "r1"}, "DELETE /orgs/o1/routes/r1", "", 204},
		{[]string{"org", "route", "reorder", "r2", "r1"}, "PUT /orgs/o1/routes/order", `{"route_ids":["r2","r1"]}`, 200},
		{[]string{"org", "route", "apply"}, "POST /orgs/o1/routes/apply", "", 204},
		{[]string{"org", "device", "list"}, "GET /orgs/o1/devices", "", 200},
		{[]string{"org", "device", "set-tags", "u1", "d1", "t1", "t2"}, "PUT /orgs/o1/members/u1/devices/d1/tags", `{"tag_ids":["t1","t2"]}`, 200},
		{[]string{"org", "billing", "get"}, "GET /orgs/o1/billing", "", 200},
		{[]string{"org", "billing", "invoices"}, "GET /orgs/o1/billing/invoices", "", 200},
		{[]string{"org", "billing", "invoice", "txn_1"}, "GET /orgs/o1/billing/invoices/txn_1/pdf", "", 200},
	} {
		resp := `{"id":"x","items":[]}`
		if tc.status == 204 {
			resp = ""
		}
		f.reply(tc.route, tc.status, resp)
		f.calls = nil
		r := runCLI(t, srv, nil, "", tc.argv...)
		if r.code != 0 {
			t.Errorf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
			continue
		}
		if len(f.calls) != 2 || f.calls[0].Method+" "+f.calls[0].Path != "GET /orgs" {
			t.Errorf("%v: calls %+v, want GET /orgs then %s", tc.argv, f.calls, tc.route)
			continue
		}
		c := f.calls[1]
		if got := c.Method + " " + c.Path; got != tc.route {
			t.Errorf("%v: sent %s, want %s", tc.argv, got, tc.route)
		}
		got := ""
		if c.Body != nil {
			got = mustJSON(t, c.Body)
		}
		if got != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, got, tc.body)
		}
		for _, c := range f.calls {
			if c.Auth != "Bearer hlo_test" {
				t.Errorf("%v: %s %s sent %q, want the org admin key", tc.argv, c.Method, c.Path, c.Auth)
			}
		}
		if strings.TrimSpace(r.stdout) != resp {
			t.Errorf("%v: piped output %q, want the raw response", tc.argv, r.stdout)
		}
	}
}

func TestOrgUsageErrors(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	for _, argv := range [][]string{
		{"org", "update"},
		{"org", "member", "invite", "ops@example.com"},
		{"org", "member", "invite", "ops@example.com", "--role", "member", "--expires-in", "90m"},
		{"org", "member", "update", "u1"},
		{"org", "member", "update", "u1", "--email", "a@b.c", "--clear-email"},
		{"org", "invite", "create", "--role", "member", "-q"},
		{"org", "key", "create", "ci", "-q"},
		{"org", "key", "roll", testKeyID, "-q"},
		{"org", "key", "create", "ci", "--notifications", "true"},
		{"org", "key", "create", "ci", "--devices", "read"},
		{"org", "key", "update", "not-a-uuid", "--widgets", "read"},
		{"org", "key", "update", testKeyID},
		{"org", "tag", "devices", "t1"},
		{"org", "tag", "devices", "t1", "--add", "d1"},
		{"org", "route", "reorder"},
		{"org", "device", "set-tags", "u1"},
		{"org", "member", "list", "--limit", "-1"},
	} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", argv, r.code, ExitUsage, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("usage errors made %d calls: %+v", len(f.calls), f.calls)
	}
}

func TestOrgKeyTokens(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1", 200, `{"id":"o1"}`)
	f.reply("GET /auth/me", 200, `{"id":"u1"}`)

	// No org admin key: refused before any request, with the way to add one.
	r := runCLI(t, srv, nil, "", "org", "get")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "pushward auth login --org") || !strings.Contains(r.stderr, config.EnvOrgToken) || len(f.calls) != 0 {
		t.Errorf("no org key: exit %d, %d calls, stderr %q", r.code, len(f.calls), r.stderr)
	}

	// The stored org admin key is used, and the environment wins over it.
	if _, err := config.Write(config.File{OrgToken: "hlo_file"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ env, want string }{{"", "Bearer hlo_file"}, {"hlo_env", "Bearer hlo_env"}} {
		t.Setenv("PUSHWARD_ORG_API_TOKEN", tc.env)
		f.calls = nil
		if r := runCLI(t, srv, nil, "", "org", "get"); r.code != 0 {
			t.Fatalf("org get: exit %d: %s", r.code, r.stderr)
		}
		for _, c := range f.calls {
			if c.Auth != tc.want {
				t.Errorf("env %q: %s sent %q, want %q", tc.env, c.Path, c.Auth, tc.want)
			}
		}
	}

	// Personal commands keep the integration key with an org key set.
	f.calls = nil
	if r := runCLI(t, srv, nil, "", "me"); r.code != 0 || len(f.calls) != 1 || f.calls[0].Auth != "Bearer hlk_test" {
		t.Errorf("me: exit %d, calls %+v", r.code, f.calls)
	}
	// And org commands never send it, even with no org key: they fail instead.
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "")
	if _, err := config.Write(config.File{Token: "hlk_file"}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if r := runCLI(t, srv, nil, "", "org", "member", "list"); r.code != ExitAuth || len(f.calls) != 0 {
		t.Errorf("org member list without an org key: exit %d, calls %+v", r.code, f.calls)
	}

	// An org admin key in the integration key slot never goes out on a
	// personal route: it fails with the way to move it.
	t.Setenv("PUSHWARD_API_TOKEN", "hlo_misplaced")
	f.calls = nil
	if r := runCLI(t, srv, nil, "", "me"); r.code != ExitAuth || len(f.calls) != 0 || !strings.Contains(r.stderr, config.EnvOrgToken) {
		t.Errorf("me with an hlo_ API key: exit %d, calls %+v, stderr %q", r.code, f.calls, r.stderr)
	}
}

func TestOrgIDFromKey(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.routes["GET /orgs/o1/members"] = func(c call) (int, string) {
		if strings.Contains(c.Query, "cursor=p2") {
			return 200, `{"items":[{"user_id":"u3"}]}`
		}
		return 200, `{"items":[{"user_id":"u1"},{"user_id":"u2"}],"next_cursor":"p2"}`
	}
	f.reply("GET /orgs", 200, testOrgs)
	r := runCLI(t, srv, nil, "", "org", "member", "list", "--jq", "[.items[].user_id] | join(\",\")")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "u1,u2,u3" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	var orgs int
	for _, c := range f.calls {
		if c.Path == "/orgs" {
			orgs++
		}
	}
	if orgs != 1 || len(f.calls) != 3 {
		t.Errorf("GET /orgs %d times in %d calls, want once in 3", orgs, len(f.calls))
	}

	// A key that lists no organization, or several, is an error.
	for _, body := range []string{`{"items":[]}`, `{"items":[{"id":"o1"},{"id":"o2"}]}`} {
		f.reply("GET /orgs", 200, body)
		f.calls = nil
		if r := runCLI(t, srv, nil, "", "org", "get"); r.code != ExitError || !strings.Contains(r.stderr, "want exactly one") || len(f.calls) != 1 {
			t.Errorf("%s: exit %d, %d calls, stderr %q", body, r.code, len(f.calls), r.stderr)
		}
	}
}

func TestOrgPagedList(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.routes["GET /orgs/o1/audit-events"] = func(c call) (int, string) {
		if strings.Contains(c.Query, "cursor=c2") {
			return 200, `{"items":[{"id":3},{"id":4}],"next_cursor":"c3"}`
		}
		return 200, `{"items":[{"id":1},{"id":2}],"next_cursor":"c2"}`
	}

	// --limit stops early: the second page asks only for what is left.
	r := runCLI(t, srv, nil, "", "org", "audit", "list", "--limit", "3")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"items":[{"id":1},{"id":2},{"id":3}]}` {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	if len(f.calls) != 3 || f.calls[1].Query != "limit=3" || f.calls[2].Query != "cursor=c2&limit=1" {
		t.Errorf("calls %+v", f.calls)
	}

	// Without it every page is read, at the largest page size.
	f.routes["GET /orgs/o1/invites"] = func(c call) (int, string) {
		if strings.Contains(c.Query, "cursor=c2") {
			return 200, `{"items":[{"id":"i3"}]}`
		}
		return 200, `{"items":[{"id":"i1"},{"id":"i2"}],"next_cursor":"c2"}`
	}
	f.calls = nil
	r = runCLI(t, srv, nil, "", "org", "invite", "list", "--jq", ".items | length")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "3" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
	if len(f.calls) != 3 || f.calls[1].Query != "limit=1000" || f.calls[2].Query != "cursor=c2&limit=1000" {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestOrgSecretsShownOnce(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("POST /orgs/o1/keys", 201, `{"id":"`+testKeyID+`","name":"ci","key":"hlk_orgnew"}`)
	f.reply("POST /orgs/o1/keys/"+testKeyID+"/roll", 200, `{"id":"`+testKeyID+`","name":"ci","key":"hlk_orgrolled"}`)

	for argv, secret := range map[string]string{"create ci": "hlk_orgnew", "roll " + testKeyID: "hlk_orgrolled"} {
		out := runTTY(t, srv, append([]string{"org", "key"}, strings.Fields(argv)...)...)
		if !strings.Contains(out, "\n"+secret+"\n") || !strings.Contains(out, "not shown again") {
			t.Errorf("org key %s printed %q", argv, out)
		}
	}

	f.reply("POST /orgs/o1/invites", 201, `{"id":"i1","role":"member","max_uses":1,"code":"JOIN-1234","expires_at":"2030-01-01T00:00:00Z"}`)
	out := runTTY(t, srv, "org", "invite", "create", "--role", "member")
	if !strings.Contains(out, "\nJOIN-1234\n") || !strings.Contains(out, "not shown again") {
		t.Errorf("invite create printed %q", out)
	}

	// The email invite's code is in the mail only; even a code in the
	// response is not printed.
	f.reply("POST /orgs/o1/invites", 201, `{"id":"i2","role":"member","email":"ops@example.com","code":"JOIN-5678","expires_at":"2030-01-01T00:00:00Z"}`)
	out = runTTY(t, srv, "org", "member", "invite", "ops@example.com", "--role", "member")
	if strings.Contains(out, "JOIN-5678") || !strings.Contains(out, "invited ops@example.com as member: pending until they join (invite i2") {
		t.Errorf("member invite printed %q", out)
	}
}

func TestOrgHumanOutput(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1/members", 200, `{"items":[{"user_id":"u1","nickname":"Maciej","role":"owner","email":"m@example.com","joined_at":"2026-01-02T03:04:05Z"}]}`)
	f.reply("GET /orgs/o1/keys", 200, `{"items":[{"id":"`+testKeyID+`","name":"ci","permissions":{"activities":"manage","notifications":"send","widgets":"none","emails":"none"},"target":{"group_ids":["g1"],"tag_ids":[]}}]}`)
	f.reply("GET /orgs/o1/invites", 200, `{"items":[
		{"id":"i1","role":"member","uses":0,"max_uses":1,"note":"a","expires_at":"2099-01-01T00:00:00Z"},
		{"id":"i2","role":"member","uses":1,"max_uses":1,"expires_at":"2099-01-01T00:00:00Z"},
		{"id":"i3","role":"viewer","uses":0,"max_uses":1,"expires_at":"2000-01-01T00:00:00Z"},
		{"id":"i4","role":"viewer","uses":0,"max_uses":1,"expires_at":"2099-01-01T00:00:00Z","revoked_at":"2026-01-01T00:00:00Z"}]}`)
	f.reply("GET /orgs/o1/routes", 200, `{"items":[{"id":"r1","position":1,"slug_pattern":"deploy-*","applies_to":"both","allow_group_ids":["g1","g2"],"allow_tag_ids":[]}]}`)
	f.reply("GET /orgs/o1/billing/invoices", 200, `{"items":[{"transaction_id":"txn_1","invoice_number":"2026-1","total_cents":4900,"currency":"EUR","status":"completed"}]}`)
	f.reply("DELETE /orgs/o1/groups/g1", 204, "")
	f.reply("PUT /orgs/o1/groups/g1/members", 200, `{"id":"g1","member_ids":["u1","u2"]}`)
	f.reply("GET /orgs/o1/audit-events", 200, `{"items":[
		{"id":2,"actor_kind":"user","actor_user_id":"u1","actor_nickname":"Maciej","action":"route.updated","target_kind":"route","target_id":"r1","created_at":"2026-01-02T03:04:05Z"},
		{"id":1,"actor_kind":"system","actor_user_id":null,"actor_nickname":null,"action":"billing.renewed","target_kind":null,"target_id":null,"created_at":"2026-01-01T03:04:05Z"}]}`)
	f.reply("GET /orgs/o1/devices", 200, `{"items":[{"user_id":"u1","nickname":"Maciej","device_id":"d1","name":"Desk iPad","platform":"ipados","tag_ids":["t1","t2"]}]}`)
	f.reply("GET /orgs/o1/groups", 200, `{"items":[{"id":"g1","name":"oncall","member_ids":["u1","u2","u3"],"description":"Primary"}]}`)
	f.reply("GET /orgs/o1/tags", 200, `{"items":[{"id":"t1","name":"wall","device_count":4,"description":"Office screens"}]}`)
	f.reply("PUT /orgs/o1/routes/order", 200, `{"items":[{"id":"r2","position":1,"slug_pattern":"ci-*","applies_to":"notifications","allow_group_ids":[],"allow_tag_ids":["t1"]},{"id":"r1","position":2,"slug_pattern":"deploy-*","applies_to":"both","allow_group_ids":["g1"],"allow_tag_ids":[]}]}`)

	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"org", "member", "list"}, []string{"USER ID", "NICKNAME", "u1", "Maciej", "owner", "m@example.com"}},
		{[]string{"org", "key", "list"}, []string{"TARGET", "activities:manage notifications:send", "groups:g1"}},
		{[]string{"org", "invite", "list"}, []string{"i1  member  pending", "i2  member  used", "i3  viewer  expired", "i4  viewer  revoked"}},
		{[]string{"org", "route", "list"}, []string{"PATTERN", "deploy-*", "both", "g1,g2"}},
		{[]string{"org", "billing", "invoices"}, []string{"txn_1", "49.00 EUR"}},
		{[]string{"org", "group", "delete", "g1"}, []string{"deleted group g1"}},
		{[]string{"org", "group", "set-members", "g1", "u1", "u2"}, []string{"group g1 has 2 members"}},
		{[]string{"org", "audit", "list"}, []string{"ACTOR", "TARGET", "user:Maciej  route.updated    route:r1", "system       billing.renewed"}},
		{[]string{"org", "device", "list"}, []string{"USER ID  MEMBER  DEVICE ID  NAME       PLATFORM  TAGS\n", "u1       Maciej  d1         Desk iPad  ipados    t1,t2"}},
		{[]string{"org", "group", "list"}, []string{"MEMBERS", "g1  oncall  3        Primary"}},
		{[]string{"org", "tag", "list"}, []string{"DEVICES", "t1  wall  4        Office screens"}},
		{[]string{"org", "route", "reorder", "r2", "r1"}, []string{"POS", "1    r2  ci-*      notifications          t1", "2    r1  deploy-*  both           g1"}},
	} {
		out := runTTY(t, srv, tc.argv...)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q:\n%s", tc.argv, w, out)
			}
		}
	}
	// --json keeps the raw response on a terminal.
	for argv, want := range map[string]string{
		"org member list":         `"user_id": "u1"`,
		"org audit list":          `"actor_kind": "system"`,
		"org device list":         `"device_id": "d1"`,
		"org group list":          `"member_ids": [`,
		"org tag list":            `"device_count": 4`,
		"org route reorder r2 r1": `"slug_pattern": "ci-*"`,
	} {
		if out := runTTY(t, srv, append(strings.Fields(argv), "--json")...); !strings.Contains(out, want) {
			t.Errorf("%s --json printed %q", argv, out)
		}
	}
}

func TestOrgInvoiceDownload(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1/billing/invoices/txn_1/pdf", 200, `{"url":"`+srv.URL+`/files/inv.pdf?sig=1"}`)
	f.reply("GET /files/inv.pdf", 200, "%PDF-1.4 test")
	path := filepath.Join(t.TempDir(), "inv.pdf")

	r := runCLI(t, srv, nil, "", "org", "billing", "invoice", "txn_1", "--output", path)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "%PDF-1.4 test" {
		t.Errorf("saved %q %v", data, err)
	}
	if last := f.calls[len(f.calls)-1]; last.Path != "/files/inv.pdf" || last.Auth != "" {
		t.Errorf("download %+v, want the link without a key", last)
	}

	// A link that is not https (or loopback http) is not followed.
	f.reply("GET /orgs/o1/billing/invoices/txn_1/pdf", 200, `{"url":"http://example.com/inv.pdf"}`)
	if r := runCLI(t, srv, nil, "", "org", "billing", "invoice", "txn_1", "--output", path); r.code != ExitError || !strings.Contains(r.stderr, "https") {
		t.Errorf("http link: exit %d %q", r.code, r.stderr)
	}
}

func TestOrgInvoiceDownloadTimeout(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	stall := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stall.Close)
	defer func(d time.Duration) { downloadTimeout = d }(downloadTimeout)
	downloadTimeout = 50 * time.Millisecond
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1/billing/invoices/txn_1/pdf", 200, `{"url":"`+stall.URL+`/inv.pdf"}`)
	r := runCLI(t, srv, nil, "", "org", "billing", "invoice", "txn_1", "--output", filepath.Join(t.TempDir(), "inv.pdf"))
	if r.code != ExitError || !strings.Contains(r.stderr, "deadline exceeded") {
		t.Errorf("stalled download: exit %d %q", r.code, r.stderr)
	}
}

func TestAuthOrgKey(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	f.routes["GET /orgs"] = func(c call) (int, string) {
		if c.Auth != "Bearer "+testOrgKey {
			return 401, `{"status":401,"detail":"bad key"}`
		}
		return 200, testOrgs
	}
	if _, err := config.Write(config.File{Token: "hlk_kept_personal_key"}); err != nil {
		t.Fatal(err)
	}

	// Checked with GET /orgs, stored in the org slot only.
	r := runCLI(t, srv, nil, testOrgKey+"\n", "auth", "login", "--org", "--with-token")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if len(f.calls) != 1 || f.calls[0].Path != "/orgs" {
		t.Errorf("login checked with %+v, want GET /orgs", f.calls)
	}
	if c := readConfig(t); c["org_token"] != testOrgKey || c["token"] != "hlk_kept_personal_key" {
		t.Errorf("config after login --org: %v", c)
	}
	if r := runCLI(t, srv, nil, "hlo_bad\n", "auth", "login", "--org", "--with-token"); r.code != ExitAuth {
		t.Errorf("rejected key: exit %d", r.code)
	}

	// Each slot refuses the other kind of key before asking the API.
	f.calls = nil
	for _, argv := range [][]string{{"auth", "login", "--org", "--with-token"}, {"auth", "login", "--with-token"}} {
		stdin := "hlk_personal\n"
		if len(argv) == 3 {
			stdin = "hlo_org\n"
		}
		r := runCLI(t, srv, nil, stdin, argv...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "pushward auth login") {
			t.Errorf("%v with %q: exit %d %q", argv, stdin, r.code, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("refused keys made %d calls", len(f.calls))
	}

	// auth status shows both slots and where each comes from.
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_env_personal_key")
	f.reply("GET /auth/me", 200, `{"id":"u1","nickname":"Maciej"}`)
	out := runTTY(t, srv, "auth", "status")
	for _, want := range []string{"key          hlk_env_...ey (from PUSHWARD_API_TOKEN)", "org key      hlo_new_...ey (from "} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_env_org_admin_key")
	if out := runTTY(t, srv, "auth", "status"); !strings.Contains(out, "org key      hlo_env_...ey (from PUSHWARD_ORG_API_TOKEN)") {
		t.Errorf("status with the org key in the environment:\n%s", out)
	}
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "")

	// logout --org clears only the org slot, plain logout only the other.
	if r := runCLI(t, srv, nil, "", "auth", "logout", "--org"); r.code != 0 {
		t.Fatalf("logout --org: exit %d %s", r.code, r.stderr)
	}
	if c := readConfig(t); c["org_token"] != "" || c["token"] != "hlk_kept_personal_key" {
		t.Errorf("config after logout --org: %v", c)
	}
	if _, err := config.Write(config.File{Token: "hlk_kept_personal_key", OrgToken: testOrgKey}); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, srv, nil, "", "auth", "logout"); r.code != 0 {
		t.Fatalf("logout: exit %d %s", r.code, r.stderr)
	}
	if c := readConfig(t); c["org_token"] != testOrgKey || c["token"] != "" {
		t.Errorf("config after logout: %v", c)
	}
}

// With only an org admin key, auth status checks that key instead of failing
// on the missing integration key.
func TestAuthStatusOrgKeyOnly(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	t.Setenv("PUSHWARD_ORG_API_TOKEN", testOrgKey)
	f.reply("GET /orgs", 200, testOrgs)
	out := runTTY(t, srv, "auth", "status")
	for _, want := range []string{"key          none\n", "org key      hlo_new_...ey (from PUSHWARD_ORG_API_TOKEN)\n", "organization Acme (o1) as admin\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	f.calls = nil
	r := runCLI(t, srv, nil, "", "auth", "status", "--jq", ".id")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "o1" || len(f.calls) != 1 || f.calls[0].Auth != "Bearer "+testOrgKey {
		t.Errorf("piped: exit %d out %q calls %+v", r.code, r.stdout, f.calls)
	}
	f.reply("GET /orgs", 401, `{"status":401,"detail":"bad key"}`)
	if r := runCLI(t, srv, nil, "", "auth", "status"); r.code != ExitAuth {
		t.Errorf("rejected org key: exit %d %q", r.code, r.stderr)
	}
}
