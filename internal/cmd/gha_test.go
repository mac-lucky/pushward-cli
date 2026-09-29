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
		"unknown":          {map[string]string{"token": "hlk", "command": "frobnicate"}, "unknown command"},
		"group only":       {map[string]string{"token": "hlk", "command": "activity"}, "unknown command"},
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
