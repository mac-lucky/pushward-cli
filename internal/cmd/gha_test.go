package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func ghaEnv(t *testing.T, inputs map[string]string) (map[string]string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GITHUB_OUTPUT":     out,
		"GITHUB_SERVER_URL": "https://github.com",
		"GITHUB_REPOSITORY": "mac-lucky/demo",
		"GITHUB_RUN_ID":     "42",
		"GITHUB_JOB":        "build",
		"GITHUB_WORKFLOW":   "CI",
	}
	for k, v := range inputs {
		env["INPUT_"+strings.ToUpper(k)] = v
	}
	return env, out
}

func readOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)([a-z-]+)<<(ghadelimiter_[0-9a-f]+)\n(.*?)\n?(ghadelimiter_[0-9a-f]+)\n`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = m[3]
	}
	return out
}

func TestGHANotify(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	f.reply("POST /notifications", 201, `{"id":5,"delivery":"all"}`)
	env, out := ghaEnv(t, map[string]string{"token": "hlk_action", "title": "CI failed", "body": "it's \"broken\" $(rm -rf /)", "level": "time-sensitive", "fields": "metadata.sha=abc\n\n"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "::add-mask::hlk_action\n") {
		t.Errorf("token not masked first: %q", r.stdout)
	}
	c := f.calls[0]
	if c.Auth != "Bearer hlk_action" {
		t.Errorf("auth %q", c.Auth)
	}
	want := `{"body":"it's \"broken\" $(rm -rf /)","level":"time-sensitive","metadata":{"sha":"abc"},"source":"github-actions","thread_id":"mac-lucky/demo","title":"CI failed","url":"https://github.com/mac-lucky/demo/actions/runs/42"}`
	if got := mustJSON(t, c.Body); got != want {
		t.Errorf("body\n got  %s\n want %s", got, want)
	}
	o := readOutputs(t, out)
	if o["id"] != "5" || o["response"] != `{"id":5,"delivery":"all"}` || o["status"] != "all" {
		t.Errorf("outputs %v", o)
	}
}

func TestGHAActivityLifecycle(t *testing.T) {
	f, srv := newFake(t)
	slug := "gha-mac-lucky-demo-42-build"
	f.reply("POST /activities", 201, `{"slug":"`+slug+`"}`)
	f.reply("PATCH /activities/"+slug, 200, `{"slug":"`+slug+`","state":"ongoing"}`)
	env, out := ghaEnv(t, map[string]string{"token": "hlk_a", "command": "activity start", "template": "steps", "fields": "content.total_steps=3"})
	if r := runCLI(t, srv, env, "", "gha"); r.code != 0 {
		t.Fatalf("start: exit %d\n%s", r.code, r.stdout)
	}
	if got := mustJSON(t, f.calls[0].Body); got != `{"ended_ttl":900,"name":"CI","slug":"`+slug+`","stale_ttl":21600}` {
		t.Errorf("create %s", got)
	}
	if got := mustJSON(t, f.calls[1].Body); got != `{"content":{"subtitle":"demo / build","template":"steps","total_steps":3,"url":"https://github.com/mac-lucky/demo/actions/runs/42"},"state":"ongoing"}` {
		t.Errorf("patch %s", got)
	}
	if o := readOutputs(t, out); o["slug"] != slug {
		t.Errorf("slug output %q", o["slug"])
	}

	// End with a job that never started: a warning, not a failure.
	f.calls = nil
	delete(f.routes, "PATCH /activities/"+slug)
	env, _ = ghaEnv(t, map[string]string{"token": "hlk_a", "command": "activity end", "status": "failure", "slug": "never-started"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 || !strings.Contains(r.stderr, "does not exist") {
		t.Errorf("end missing: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if f.calls[0].Path != "/activities/never-started" {
		t.Errorf("slug input ignored: %s", f.calls[0].Path)
	}
}

func TestGHAFailureAnnotates(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 422, `{"status":422,"code":"validation","detail":"title is required"}`)
	env, _ := ghaEnv(t, map[string]string{"token": "hlk_a", "body": "b"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != ExitError || !strings.Contains(r.stdout, "::error title=PushWard::POST /notifications: 422 validation: title is required") {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
	if strings.Contains(r.stderr, "error:") {
		t.Errorf("error printed twice: %q", r.stderr)
	}
	env["INPUT_FAIL-ON-ERROR"] = "false"
	r = runCLI(t, srv, env, "", "gha")
	if r.code != 0 || !strings.Contains(r.stdout, "::warning title=PushWard::") {
		t.Errorf("fail-on-error false: exit %d\n%s", r.code, r.stdout)
	}
}

func TestGHAGuards(t *testing.T) {
	_, srv := newFake(t)
	for name, tc := range map[string]struct {
		inputs map[string]string
		want   string
	}{
		"no token":         {map[string]string{"command": "notify"}, "token input is required"},
		"auth refused":     {map[string]string{"token": "hlk", "command": "auth login"}, "not available"},
		"e2e refused":      {map[string]string{"token": "hlk", "command": "e2e generate"}, "not available"},
		"encrypt, no key":  {map[string]string{"token": "hlk", "title": "a", "body": "b", "encrypt": "true"}, "no encryption key"},
		"flag before e2e":  {map[string]string{"token": "hlk", "command": "--json e2e decrypt pw1.x"}, "not available"},
		"short flag e2e":   {map[string]string{"token": "hlk", "command": "-q e2e generate"}, "not available"},
		"jq before auth":   {map[string]string{"token": "hlk", "command": "--jq .x auth status"}, "not available"},
		"answers follow":   {map[string]string{"token": "hlk", "command": "notification answers --follow"}, "--follow is not available"},
		"answers follow=1": {map[string]string{"token": "hlk", "command": "notification answers --after 57 --follow=true"}, "--follow is not available"},
		"unknown":          {map[string]string{"token": "hlk", "command": "frobnicate"}, "unknown command"},
		"group only":       {map[string]string{"token": "hlk", "command": "activity"}, "unknown command"},
		"subgroup only":    {map[string]string{"token": "hlk", "command": "org member"}, "unknown command"},
		"url in command":   {map[string]string{"token": "hlk", "command": "notify --api-url https://evil.example"}, "api-url input"},
		"bad quoting":      {map[string]string{"token": "hlk", "command": `notify "unterminated`}, "command input"},
		"widget no slug":   {map[string]string{"token": "hlk", "command": "widget update"}, "needs a slug"},
		"schedule no id":   {map[string]string{"token": "hlk", "command": "schedule cancel"}, "needs an id"},
		"flag in command":  {map[string]string{"token": "hlk", "command": "notify --nope"}, "unknown flag"},
		"ignored input ok": {map[string]string{"token": "hlk", "command": "health", "title": "x"}, "is ignored by"},
	} {
		env, _ := ghaEnv(t, tc.inputs)
		r := runCLI(t, srv, env, "", "gha")
		if !strings.Contains(r.stdout, tc.want) {
			t.Errorf("%s: stdout %q, want %q", name, r.stdout, tc.want)
		}
	}
}

// --follow never ends, so the Action refuses it before any request; --wait
// is the way to wait there.
func TestGHAAnswersFollowRefused(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /notifications/answers", 200, `{"items":[{"notification_id":9,"action_id":"go"}],"cursor":"58"}`)
	env, _ := ghaEnv(t, map[string]string{"token": "hlk_a", "command": "notification answers --after 57 --follow"})
	if r := runCLI(t, srv, env, "", "gha"); r.code != ExitUsage || len(f.calls) != 0 {
		t.Errorf("--follow: exit %d after %d calls\n%s", r.code, len(f.calls), r.stdout)
	}
	for _, command := range []string{"notification answers --after 57 --wait 1m", "notification answers --after 57 --follow=false"} {
		env, _ := ghaEnv(t, map[string]string{"token": "hlk_a", "command": command})
		if r := runCLI(t, srv, env, "", "gha"); r.code != 0 {
			t.Errorf("%s: exit %d\n%s", command, r.code, r.stdout)
		}
	}
}

func TestGHAMasksNewKey(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	f.reply("POST /integrations/keys/"+testKeyID+"/roll", 200, `{"id":"`+testKeyID+`","key":"hlk_rolled"}`)
	env, _ := ghaEnv(t, map[string]string{"token": "hlk_action", "command": "key roll " + testKeyID})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	mask, echo := strings.Index(r.stdout, "::add-mask::hlk_rolled\n"), strings.Index(r.stdout, "::group::")
	if mask < 0 || echo < 0 || mask > echo {
		t.Errorf("new key not masked before the response is logged:\n%s", r.stdout)
	}
}

// The codes org invite create and share grant return are shown once, and an
// invoice link carries a token, so they are masked before the response
// reaches the log.
func TestGHAMasksCodes(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("POST /orgs/o1/invites", 201, `{"id":"i1","role":"admin","max_uses":5,"code":"JOIN-1234"}`)
	f.reply("GET /orgs/o1/billing/invoices/txn_1/pdf", 200, `{"url":"https://example.com/invoice.pdf?token=t1"}`)
	f.reply("POST /activities/deploy/permissions", 201, `{"id":"p1","role":"viewer","activity_slug":"deploy","grantee":{"type":"share_link","share_link":{"code":"SHR-9","share_url":"https://pushward.app/s/SHR-9"}}}`)
	f.reply("POST /permissions/patterns", 201, `{"id":"p2","role":"viewer","slug_pattern":"ci-*","grantee":{"type":"pattern_link","pattern_link":{"code":"PAT-7","share_url":"https://pushward.app/s/PAT-7"}}}`)
	for command, secrets := range map[string][]string{
		"org invite create --role admin --max-uses 5": {"JOIN-1234"},
		"org billing invoice txn_1":                   {"https://example.com/invoice.pdf?token=t1"},
		"share grant deploy":                          {"SHR-9", "https://pushward.app/s/SHR-9"},
		"share grant-pattern ci-*":                    {"PAT-7", "https://pushward.app/s/PAT-7"},
	} {
		env, _ := ghaEnv(t, map[string]string{"token": "hlk_action", "command": command})
		r := runCLI(t, srv, env, "", "gha")
		if r.code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", command, r.code, r.stdout, r.stderr)
		}
		echo := strings.Index(r.stdout, "::group::")
		for _, s := range secrets {
			if mask := strings.Index(r.stdout, "::add-mask::"+s+"\n"); mask < 0 || echo < 0 || mask > echo {
				t.Errorf("%s: %s not masked before the response is logged:\n%s", command, s, r.stdout)
			}
		}
	}
}

// me has subcommands but is a command itself, so the Action runs it.
func TestGHAMe(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /auth/me", 200, `{"id":"u1","live_activity_updates_used":3}`)
	f.reply("GET /app/onboarding", 200, `{"use_case":"github"}`)
	for command, path := range map[string]string{"me --jq .live_activity_updates_used": "/auth/me", "me onboarding": "/app/onboarding"} {
		f.calls = nil
		env, _ := ghaEnv(t, map[string]string{"token": "hlk_action", "command": command})
		r := runCLI(t, srv, env, "", "gha")
		if r.code != 0 || len(f.calls) != 1 || f.calls[0].Path != path {
			t.Errorf("%s: exit %d, calls %+v\n%s", command, r.code, f.calls, r.stdout)
		}
	}
}

func TestGHAMasksCallbackSecret(t *testing.T) {
	_, srv := newFake(t)
	// --key derives it from another key than the token, which the run masks
	// up front.
	const secret = "whsec_I1p3Yj83UaqkDUbjL1juMcCeRaMo2WAxaTS/hpRZYfA="
	env, out := ghaEnv(t, map[string]string{"token": "hlk_action", "command": "receipt secret --key hlk_0123456789abcdef0123456789abcdef"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	echo := strings.Index(r.stdout, "::group::")
	for _, s := range []string{secret, strings.TrimPrefix(secret, "whsec_")} {
		if mask := strings.Index(r.stdout, "::add-mask::"+s+"\n"); mask < 0 || echo < 0 || mask > echo {
			t.Errorf("%s not masked before the response is logged:\n%s", s, r.stdout)
		}
	}
	if o := readOutputs(t, out); o["response"] != `{"secret":"`+secret+`"}` {
		t.Errorf("outputs %v", o)
	}
}

func TestGHAWaitForAnswer(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":8,"answerable":true}`)
	f.reply("GET /notifications/answers/8", 200, `{"notification_id":8,"status":"answered","action_id":"skip","text":"not today"}`)
	env, out := ghaEnv(t, map[string]string{"token": "hlk", "title": "Deploy?", "body": "v2", "actions": "deploy=Deploy\nskip=Skip", "wait": "5m"})
	if r := runCLI(t, srv, env, "", "gha"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if got := mustJSON(t, f.calls[0].Body["actions"]); got != `[{"id":"deploy","title":"Deploy"},{"id":"skip","title":"Skip"}]` {
		t.Errorf("actions %s", got)
	}
	o := readOutputs(t, out)
	if o["answer"] != "skip" || o["answer-text"] != "not today" || o["id"] != "8" || o["status"] != "answered" {
		t.Errorf("outputs %v", o)
	}
}

func TestGHAEncrypts(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", "")
	f.reply("POST /notifications", 201, `{"id":5,"delivery":"all"}`)
	env, _ := ghaEnv(t, map[string]string{"token": "hlk_action", "e2e-key": strings.ToUpper(testE2EKey),
		"title": "Prod DB password rotated", "body": "new one in the vault", "fields": "subtitle=db/prod"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, mask := range []string{"::add-mask::" + strings.ToUpper(testE2EKey) + "\n", "::add-mask::" + testE2EKey + "\n"} {
		if !strings.Contains(r.stdout, mask) {
			t.Errorf("key not masked as %q:\n%s", mask, r.stdout)
		}
	}
	for _, plain := range []string{"rotated", "vault", "db/prod", "actions/runs"} {
		if strings.Contains(r.stdout, plain) {
			t.Errorf("%q in the log:\n%s", plain, r.stdout)
		}
	}
	if !strings.Contains(r.stdout, "--title=*** --body=*** --raw-field=*** --url=***") {
		t.Errorf("debug line not redacted:\n%s", r.stdout)
	}
	m := openSent(t, testE2EKey, f.calls[0].Body)
	if m.Title != "Prod DB password rotated" || m.Subtitle != "db/prod" || m.URL != "https://github.com/mac-lucky/demo/actions/runs/42" {
		t.Errorf("sealed %+v", m)
	}
	if f.calls[0].Body["source"] != "github-actions" {
		t.Errorf("body %v", f.calls[0].Body)
	}
}

func TestRedactSealed(t *testing.T) {
	got := strings.Join(redactSealed([]string{"notify", "--title", "a", "--body=b", "-d", "{}", "-fsubtitle=c", "--level", "active", "-F", "x=1", "--", "--title"}), " ")
	if want := "notify --title *** --body=*** -d *** -f*** --level active -F *** -- --title"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGHAAck(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":9,"receipt":{"notification_id":9,"status":"active"}}`)
	f.reply("GET /notifications/receipts/9", 200, `{"notification_id":9,"status":"acknowledged","action_id":"pw_ack"}`)
	env, out := ghaEnv(t, map[string]string{"token": "hlk_0123456789abcdef0123456789abcdef", "title": "a", "body": "b",
		"ack": "true", "ack-repeat": "2m", "tags": "deploy\nprod\n", "callback-url": "https://hooks.example.com/pw", "wait": "5m"})
	r := runCLI(t, srv, env, "", "gha")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	b := f.calls[0].Body
	if mustJSON(t, b["acknowledge"]) != `{"repeat_seconds":120}` || mustJSON(t, b["tags"]) != `["deploy","prod"]` || b["callback_url"] != "https://hooks.example.com/pw" {
		t.Errorf("body %v", b)
	}
	for _, mask := range []string{"whsec_I1p3Yj83UaqkDUbjL1juMcCeRaMo2WAxaTS/hpRZYfA=", "I1p3Yj83UaqkDUbjL1juMcCeRaMo2WAxaTS/hpRZYfA="} {
		if !strings.Contains(r.stdout, "::add-mask::"+mask+"\n") {
			t.Errorf("callback secret not masked as %s:\n%s", mask, r.stdout)
		}
	}
	if o := readOutputs(t, out); o["id"] != "9" || o["status"] != "acknowledged" || o["answer"] != "pw_ack" {
		t.Errorf("outputs %v", o)
	}
}

func TestGHACancelTags(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications/receipts/cancel", 200, `{"canceled":1}`)
	env, out := ghaEnv(t, map[string]string{"token": "hlk_a", "command": "receipt cancel", "tags": "deploy\nprod\n"})
	if r := runCLI(t, srv, env, "", "gha"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if len(f.calls) != 2 || f.calls[0].Body["tag"] != "deploy" || f.calls[1].Body["tag"] != "prod" {
		t.Errorf("calls %v", f.calls)
	}
	if o := readOutputs(t, out); o["response"] != `{"canceled":2}` {
		t.Errorf("outputs %v", o)
	}
}

func TestGHAPresealed(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", "")
	f.reply("POST /notifications", 201, `{"id":5,"delivery":"all"}`)
	env := vectorEnvelope(t)
	for name, inputs := range map[string]map[string]string{
		"fields":  {"fields": "encrypted=" + env},
		"json":    {"json": `{"encrypted":"` + env + `"}`},
		"command": {"command": "notify -f encrypted=" + env},
	} {
		inputs["token"] = "hlk_action"
		genv, _ := ghaEnv(t, inputs)
		f.calls = nil
		r := runCLI(t, srv, genv, "", "gha")
		if r.code != 0 || len(f.calls) != 1 {
			t.Fatalf("%s: exit %d\n%s", name, r.code, r.stdout)
		}
		want := `{"encrypted":"` + env + `","source":"github-actions","thread_id":"mac-lucky/demo"}`
		if got := mustJSON(t, f.calls[0].Body); got != want {
			t.Errorf("%s: body\n got  %s\n want %s", name, got, want)
		}
	}
}

func TestGHARefusesUnsealed(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", "")
	f.reply("GET /notifications/receipts/9", 200, `{"notification_id":9,"status":"active"}`)
	f.reply("GET /activities", 200, `{"items":[]}`)
	post := "api -X POST /notifications -f title=X -f body=Y"
	for name, tc := range map[string]struct {
		inputs map[string]string
		env    string
		ok     bool
	}{
		"encrypt, api":           {inputs: map[string]string{"encrypt": "true", "command": post}},
		"encrypt, receipt":       {inputs: map[string]string{"encrypt": "true", "command": "receipt get 9"}},
		"encrypt false, receipt": {inputs: map[string]string{"encrypt": "false", "command": "receipt get 9"}, ok: true},
		"key, api":               {inputs: map[string]string{"command": post}, env: testE2EKey},
		"key input, api":         {inputs: map[string]string{"e2e-key": testE2EKey, "command": post}},
		"key, api scheduled":     {inputs: map[string]string{"e2e-key": testE2EKey, "command": "api notifications/scheduled -f title=X"}},
		"key, api dotted path":   {inputs: map[string]string{"e2e-key": testE2EKey, "command": "api /activities/../Notifications/ -f title=X"}},
		"key, api escaped path":  {inputs: map[string]string{"e2e-key": testE2EKey, "command": "api /%6eotifications -f title=X"}},
		"key, receipt get":       {inputs: map[string]string{"e2e-key": testE2EKey, "command": "receipt get 9"}, ok: true},
		"key, api other path":    {inputs: map[string]string{"e2e-key": testE2EKey, "command": "api /activities"}, ok: true},
		"no key, api":            {inputs: map[string]string{"command": "api /notifications/receipts/9"}, ok: true},
	} {
		tc.inputs["token"] = "hlk_action"
		env, _ := ghaEnv(t, tc.inputs)
		env["PUSHWARD_E2E_KEY"] = tc.env
		f.calls = nil
		r := runCLI(t, srv, env, "", "gha")
		switch {
		case tc.ok && (r.code != 0 || len(f.calls) != 1):
			t.Errorf("%s: exit %d, %d calls\n%s", name, r.code, len(f.calls), r.stdout)
		case !tc.ok && (r.code != ExitUsage || len(f.calls) != 0 || !strings.Contains(r.stdout, "encryption was requested but pushward ")):
			t.Errorf("%s: exit %d, %d calls\n%s", name, r.code, len(f.calls), r.stdout)
		}
	}
}
