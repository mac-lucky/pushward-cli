package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hookPermission = `{"session_id":"s1","cwd":"/home/u/webapp","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make deploy"}}`

func agentSlug(session, tool, canonicalInput string) string {
	h := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:4])
	}
	return "agent-" + h(session) + "-" + h(tool+"\n"+canonicalInput)
}

// createRoute answers POST /activities the way the server does for a
// personal key, or for an organization's when org is set: a create with a
// target (the key check) is refused, any other gets status and body.
func createRoute(f *fakeAPI, org bool, status int, body string) {
	f.routes["POST /activities"] = targetRoute(org, status, body)
}

// notifyRoute is createRoute for POST /notifications.
func notifyRoute(f *fakeAPI, org bool, status int, body string) {
	f.routes["POST /notifications"] = targetRoute(org, status, body)
}

func targetRoute(org bool, status int, body string) func(call) (int, string) {
	return func(c call) (int, string) {
		switch {
		case c.Body["target"] == nil:
			return status, body
		case org:
			return 422, `{"status":422,"code":"org.target_invalid","detail":"target must name at least one group, tag or member"}`
		}
		return 422, `{"status":422,"code":"org.target_unavailable","detail":"target is for organization keys"}`
	}
}

const permissionDenied = `{"status":403,"code":"integration_key.permission_denied","detail":"this key may not do that"}`

// sentTo is every request to method and path, in order.
func sentTo(f *fakeAPI, method, path string) []call {
	var out []call
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func TestAgentHookAnswersFromTheCard(t *testing.T) {
	f, srv := newFake(t)
	slug := agentSlug("s1", "Bash", `{"command":"make deploy"}`)
	createRoute(f, false, 201, `{"slug":"`+slug+`","state":"ongoing","content":{}}`)
	f.reply("PATCH /activities/"+slug, 200, `{"slug":"`+slug+`","state":"ongoing"}`)
	f.reply("GET /activities/"+slug, 200, `{"slug":"`+slug+`","state":"ongoing","content":{"template":"approval","answer":{"option":"deny","by":"user","at":1}}}`)
	r := runCLI(t, srv, nil, hookPermission, "agent", "hook", "claude", "--desk", "away")
	if r.code != 0 || r.stdout != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Denied from PushWard"}}}`+"\n" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(r.stderr, "make deploy") {
		t.Errorf("the log holds the command: %s", r.stderr)
	}
	creates := sentTo(f, "POST", "/activities")
	if len(creates) != 2 || creates[0].Body["slug"] != "agent-key-check" || creates[1].Body["slug"] != slug {
		t.Fatalf("creates %+v", creates)
	}
	if card := sentTo(f, "PATCH", "/activities/"+slug); len(card) != 1 || card[0].Body["content"].(map[string]any)["state"] != "Run a command" {
		t.Errorf("card %+v", card)
	}
}

// With an organization's key the hook sends nothing: other members would get
// the card and could answer it.
func TestAgentHookRefusesOrganizationKeys(t *testing.T) {
	f, srv := newFake(t)
	createRoute(f, true, 201, `{}`)
	r := runCLI(t, srv, nil, hookPermission, "agent", "hook", "claude", "--desk", "away")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "organization's key") {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	if len(f.calls) != 1 {
		t.Errorf("%d requests, want the key check alone: %+v", len(f.calls), f.calls)
	}
	r = runCLI(t, srv, nil, "", "agent", "test")
	if r.code == 0 || !strings.Contains(r.stderr, "personal key") {
		t.Errorf("agent test: exit %d stderr %s", r.code, r.stderr)
	}

	// An organization's key that may send notifications but not manage
	// activities: the notification fallback would reach the organization.
	f.calls = nil
	f.reply("POST /activities", 403, permissionDenied)
	notifyRoute(f, true, 201, `{"id":1,"answerable":true}`)
	f.reply("GET /notifications/answers/1", 200, `{"notification_id":1,"status":"answered","action_id":"allow"}`)
	r = runCLI(t, srv, nil, hookPermission, "agent", "hook", "claude", "--desk", "away")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "organization's key") {
		t.Fatalf("notifications-only key: exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	for _, c := range f.calls {
		if c.Body["target"] == nil {
			t.Errorf("notifications-only key sent %s %s %v", c.Method, c.Path, c.Body)
		}
	}
}

// With an encryption key the fallback notification is sealed, and a sealed
// send the server refuses is not sent again readable.
func TestAgentHookSealsNotifications(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	createRoute(f, false, 201, `{}`)
	f.reply("POST /notifications", 201, `{"id":5,"answerable":true}`)
	f.reply("GET /notifications/answers/5", 200, `{"notification_id":5,"status":"answered","action_id":"allow"}`)
	r := runCLI(t, srv, nil, hookPermission, "agent", "hook", "codex", "--desk", "away", "--via", "notification")
	if r.code != 0 || r.stdout != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`+"\n" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	sent := sentTo(f, "POST", "/notifications")
	if len(sent) != 1 {
		t.Fatalf("%d notifications", len(sent))
	}
	if m := openSent(t, testE2EKey, sent[0].Body); m.Title != "Codex asks" || m.Body != "Run: make deploy" || m.Subtitle != "webapp" {
		t.Errorf("sealed %+v", m)
	}
	if sent[0].Body["actions"] == nil || sent[0].Body["collapse_id"] == nil {
		t.Errorf("plaintext fields lost: %v", sent[0].Body)
	}

	f.calls = nil
	f.reply("POST /notifications", 422, `{"status":422,"code":"notification.encryption_unavailable","detail":"no encryption for organizations"}`)
	r = runCLI(t, srv, nil, `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"secret summary"}`, "agent", "hook", "claude", "--desk", "away")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("exit %d stdout %q", r.code, r.stdout)
	}
	sent = sentTo(f, "POST", "/notifications")
	if len(sent) != 1 || sent[0].Body["encrypted"] == nil || sent[0].Body["body"] != nil {
		t.Errorf("notices %+v, want one sealed attempt", sent)
	}
}

// PUSHWARD_AGENT_<FLAG> sets a flag the command line leaves out.
func TestAgentHookFlagsFromEnv(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":5,"answerable":true}`)
	f.reply("GET /notifications/answers/5", 200, `{"notification_id":5,"status":"answered","action_id":"allow"}`)
	env := map[string]string{"PUSHWARD_AGENT_VIA": "notification", "PUSHWARD_AGENT_DESK": "away"}
	r := runCLI(t, srv, env, strings.Replace(hookPermission, `"s1"`, `"s2"`, 1), "agent", "hook", "codex")
	if r.code != 0 || r.stdout != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`+"\n" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c.Path, "/activities") && c.Body["target"] == nil {
			t.Errorf("--via notification from the env still made a card: %s %s", c.Method, c.Path)
		}
	}
	// The command line wins over the environment.
	r = runCLI(t, srv, map[string]string{"PUSHWARD_AGENT_DESK": "away"}, hookPermission, "agent", "hook", "claude", "--desk", "desk")
	if r.code != 0 || r.stdout != "" {
		t.Errorf("--desk desk: exit %d stdout %q", r.code, r.stdout)
	}
}

// Whatever goes wrong, a hook exits 0 and prints nothing, so the agent's
// own prompt shows.
func TestAgentHookFailuresLeaveThePromptToTheTerminal(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /activities", 401, `{"status":401,"code":"auth.invalid","detail":"bad key"}`)
	f.reply("POST /notifications", 401, `{"status":401,"code":"auth.invalid","detail":"bad key"}`)
	closed := httptest.NewServer(nil)
	closed.Close()
	for name, c := range map[string]struct {
		env  map[string]string
		argv []string
	}{
		"401":        {nil, []string{"agent", "hook", "claude", "--desk", "away"}},
		"no key":     {map[string]string{"PUSHWARD_API_TOKEN": ""}, []string{"agent", "hook", "claude", "--desk", "away"}},
		"network":    {map[string]string{"PUSHWARD_API_URL": closed.URL}, []string{"agent", "hook", "claude", "--desk", "away"}},
		"bad flag":   {nil, []string{"agent", "hook", "claude", "--via", "email"}},
		"bad env":    {map[string]string{"PUSHWARD_AGENT_MAX_WAIT": "forever"}, []string{"agent", "hook", "claude"}},
		"no flag":    {nil, []string{"agent", "hook", "claude", "--nope"}},
		"no agent":   {nil, []string{"agent", "hook", "gemini"}},
		"extra args": {nil, []string{"agent", "hook", "codex", "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			r := runCLI(t, srv, c.env, hookPermission, c.argv...)
			if r.code != 0 || r.stdout != "" {
				t.Errorf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
			}
		})
	}
	// Codex's Stop still gets its {} without a key.
	t.Setenv("PUSHWARD_API_TOKEN", "")
	r := runCLI(t, srv, nil, `{"session_id":"s","hook_event_name":"Stop","last_assistant_message":null}`, "agent", "hook", "codex", "--desk", "away")
	if r.code != 0 || r.stdout != "{}\n" {
		t.Errorf("codex stop: exit %d stdout %q", r.code, r.stdout)
	}
}

func TestAgentHookLogFile(t *testing.T) {
	f, srv := newFake(t)
	createRoute(f, false, 201, `{}`)
	f.reply("POST /notifications", 201, `{"id":1}`)
	logPath := filepath.Join(t.TempDir(), "agent.log")
	r := runCLI(t, srv, nil, `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"secret summary"}`,
		"agent", "hook", "claude", "--desk", "away", "--log", logPath)
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	b, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(b), "notice: ok") || strings.Contains(string(b), "secret") {
		t.Errorf("log %q %v", b, err)
	}
	if got := sentTo(f, "POST", "/notifications"); len(got) != 1 || got[0].Body["title"] != "Claude Code finished" || got[0].Body["body"] != "secret summary" {
		t.Errorf("notice %+v", got)
	}
}

// A key that passed the check is not checked again by the next hook process
// for an hour; agent status checks it every time.
func TestAgentHookRemembersTheKeyCheck(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /auth/me", 200, `{"id":"u1"}`)
	createRoute(f, false, 201, `{}`)
	f.reply("POST /notifications", 201, `{"id":1}`)
	checks := func() int {
		n := 0
		for _, c := range sentTo(f, "POST", "/activities") {
			if c.Body["slug"] == "agent-key-check" {
				n++
			}
		}
		return n
	}
	stop := `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"done"}`
	for i := range 2 {
		if r := runCLI(t, srv, nil, stop, "agent", "hook", "claude", "--desk", "away"); r.code != 0 || checks() != 1 {
			t.Fatalf("run %d: exit %d, %d key checks, stderr %s", i, r.code, checks(), r.stderr)
		}
	}
	if n := len(sentTo(f, "POST", "/notifications")); n != 2 {
		t.Errorf("%d notices, want 2", n)
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("PUSHWARD_CONFIG_DIR"), "agent-key-check.json"))
	if err != nil || strings.Contains(string(b), "hlk_test") {
		t.Errorf("cache %q %v", b, err)
	}
	if r := runCLI(t, srv, nil, "", "agent", "status"); !strings.Contains(r.stdout, "works") || checks() != 2 {
		t.Errorf("status: %d key checks\n%s", checks(), r.stdout)
	}
}

func TestAgentInstallCommand(t *testing.T) {
	_, srv := newFake(t)
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte("{\n  \"model\": \"opus\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, srv, nil, "", "agent", "install", "claude", "--settings", settings, "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, " agent hook claude\"") || !strings.Contains(r.stdout, `"timeout": 1560`) {
		t.Fatalf("dry run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if b, _ := os.ReadFile(settings); string(b) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Fatalf("--dry-run wrote the file:\n%s", b)
	}

	r = runCLI(t, srv, nil, "", "agent", "install", "claude", "--settings", settings, "--max-wait", "40m")
	if r.code != 0 || !strings.Contains(r.stdout, "added the PushWard hooks to") || !strings.Contains(r.stdout, settings+".bak") {
		t.Fatalf("install: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), ` agent hook claude --max-wait 40m0s"`) || !strings.Contains(string(b), `"timeout": 2460`) {
		t.Errorf("settings:\n%s", b)
	}
	r = runCLI(t, srv, nil, "", "agent", "install", "claude", "--settings", settings, "--max-wait", "40m")
	if !strings.Contains(r.stdout, "already has the PushWard hooks") {
		t.Errorf("second install: %s", r.stdout)
	}
	r = runCLI(t, srv, nil, "", "agent", "uninstall", "claude", "--settings", settings)
	if b, _ := os.ReadFile(settings); r.code != 0 || string(b) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Errorf("uninstall: exit %d\n%s", r.code, b)
	}

	codex := filepath.Join(dir, "codex", "hooks.json")
	r = runCLI(t, srv, nil, "", "agent", "install", "codex", "--settings", codex)
	if r.code != 0 || !strings.Contains(r.stdout, "/hooks") {
		t.Errorf("codex install: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, srv, nil, "", "agent", "install", "codex", "--settings", codex, "--max-wait", "0s")
	if r.code != ExitUsage {
		t.Errorf("--max-wait 0s: exit %d", r.code)
	}
}

func TestAgentStatus(t *testing.T) {
	f, srv := newFake(t)
	f.reply("GET /auth/me", 200, `{"id":"u1"}`)
	createRoute(f, false, 201, `{}`)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	r := runCLI(t, srv, nil, "", "agent", "install", "codex")
	if r.code != 0 {
		t.Fatalf("install: %s", r.stderr)
	}
	r = runCLI(t, srv, nil, "", "agent", "status")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"key      from $PUSHWARD_API_TOKEN, works", "desk     ", "claude   no hooks in", "codex    PermissionRequest, Stop in"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	// The desk line follows what the hook reads from the environment.
	for _, c := range []struct{ name, value, want string }{
		{"PUSHWARD_AGENT_DESK", "away", "desk     away (PUSHWARD_AGENT_DESK=away): prompts go to the phone"},
		{"PUSHWARD_AGENT_DESK", "desk", "desk     at the desk (PUSHWARD_AGENT_DESK=desk): prompts stay in the terminal"},
		{"PUSHWARD_AGENT_DESK", "sofa", "desk     --desk: want one of"},
		{"PUSHWARD_AGENT_DESK_IDLE", "a while", "desk     PUSHWARD_AGENT_DESK_IDLE: "},
	} {
		if r = runCLI(t, srv, map[string]string{c.name: c.value}, "", "agent", "status"); !strings.Contains(r.stdout, c.want) {
			t.Errorf("%s=%s: status lacks %q:\n%s", c.name, c.value, c.want, r.stdout)
		}
	}
	createRoute(f, true, 201, `{}`)
	if r = runCLI(t, srv, nil, "", "agent", "status"); !strings.Contains(r.stdout, "an organization's key: the hooks need a personal one") {
		t.Errorf("organization key:\n%s", r.stdout)
	}
	// Refused the activity before its target is read, the notification
	// check tells; refused that too, nothing does.
	f.reply("POST /activities", 403, permissionDenied)
	notifyRoute(f, true, 201, `{"id":1}`)
	if r = runCLI(t, srv, nil, "", "agent", "status"); !strings.Contains(r.stdout, "an organization's key: the hooks need a personal one") {
		t.Errorf("notifications-only organization key:\n%s", r.stdout)
	}
	notifyRoute(f, false, 201, `{"id":1}`)
	if r = runCLI(t, srv, nil, "", "agent", "status"); !strings.Contains(r.stdout, "key      from $PUSHWARD_API_TOKEN, works") {
		t.Errorf("notifications-only personal key:\n%s", r.stdout)
	}
	f.reply("POST /notifications", 403, permissionDenied)
	if r = runCLI(t, srv, nil, "", "agent", "status"); !strings.Contains(r.stdout, "could not tell whether this is a personal key; the agent hooks send nothing with it (POST /notifications: 403 integration_key.permission_denied)") {
		t.Errorf("unchecked key:\n%s", r.stdout)
	}
	f.reply("GET /auth/me", 401, `{"status":401,"code":"auth.invalid"}`)
	if r = runCLI(t, srv, nil, "", "agent", "status"); r.code != ExitAuth || !strings.Contains(r.stdout, "refused") {
		t.Errorf("refused key: exit %d\n%s", r.code, r.stdout)
	}
}

func TestAgentTestCommand(t *testing.T) {
	f, srv := newFake(t)
	createRoute(f, false, 201, `{}`)
	f.reply("POST /notifications", 201, `{"id":1,"answerable":true}`)
	f.routes["GET /notifications/answers/1"] = func(call) (int, string) {
		// Every question's notification goes out as id 1 here; answer the
		// last one sent.
		last := f.calls[len(f.calls)-2].Body
		actions := last["actions"].([]any)
		return 200, `{"notification_id":1,"status":"answered","action_id":"` + actions[1].(map[string]any)["id"].(string) + `"}`
	}
	r := runCLI(t, srv, nil, "", "agent", "test", "--via", "notification")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if r.stdout != `[{"question":"Which database should the demo use?","answer":"SQLite"},{"question":"Run the migrations now?","answer":"Not yet"},{"question":"How should the release go out?","answer":"Draft only"}]`+"\n" {
		t.Errorf("stdout %s", r.stdout)
	}
	titles := []string{}
	for _, c := range sentTo(f, "POST", "/notifications") {
		titles = append(titles, c.Body["title"].(string))
	}
	if strings.Join(titles, "|") != "Claude Code asks (1/3)|Claude Code asks (2/3)|Claude Code asks (3/3)" {
		t.Errorf("titles %v", titles)
	}
}
