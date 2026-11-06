package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	bashCommand = "npm run deploy -- --target prod-eu-7f3a"
	suggestion  = `{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm run deploy:*"}],"behavior":"allow","destination":"localSettings"}`

	claudePermission = `{"session_id":"sess-1","transcript_path":"/tmp/t.jsonl","cwd":"/home/u/src/webapp","permission_mode":"default","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"` + bashCommand + `","description":"Deploy"},"permission_suggestions":[` + suggestion + `]}`
	codexPermission  = `{"session_id":"cx-1","turn_id":"turn-1","transcript_path":null,"cwd":"/home/u/api","hook_event_name":"PermissionRequest","model":"gpt-5","permission_mode":"default","tool_name":"Bash","tool_input":{"command":["bash","-lc","cargo build --release"],"description":null},"permission_suggestions":[` + suggestion + `]}`

	allowOut  = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"
	alwaysOut = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[` + suggestion + `]}}}` + "\n"
	denyOut   = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Denied from PushWard"}}}` + "\n"
)

// questionsInput is an AskUserQuestion call; the second question's first
// label is longer than a card button.
const questionsJSON = `[{"question":"Which database should we use?","header":"Database","options":[{"label":"Postgres","description":"Relational, already in prod"},{"label":"SQLite","description":"One file"},{"label":"DynamoDB","description":"Managed"}],"multiSelect":false},{"question":"Ship it today?","header":"Release","options":[{"label":"Yes, tag and push it to production now","description":""},{"label":"No","description":"Wait for Monday"}],"multiSelect":false},{"question":"Notify the team?","header":"Notify","options":[{"label":"Slack","description":""},{"label":"Email","description":""}],"multiSelect":false}]`

func askInput(questions string) string {
	return `{"session_id":"sess-1","cwd":"/home/u/src/webapp","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":` + questions + `},"tool_use_id":"toolu_1"}`
}

// run runs one hook in a synctest bubble, so its waits take no real time.
func run(t *testing.T, f *fakeAPI, opts Options, idle Idler, agent Agent, stdin string) (string, string) {
	t.Helper()
	var out, logs string
	synctest.Test(t, func(t *testing.T) {
		r, lb := newRunner(f, opts)
		r.Idle = idle
		out = string(r.Hook(context.Background(), agent, strings.NewReader(stdin)))
		logs = lb.String()
	})
	return out, logs
}

func answerCard(option string, after time.Duration) func(int) (string, time.Duration) {
	return func(int) (string, time.Duration) { return option, after }
}

func TestClaudePermission(t *testing.T) {
	for option, want := range map[string]string{"allow": allowOut, "always": alwaysOut, "deny": denyOut} {
		t.Run(option, func(t *testing.T) {
			f := newFakeAPI()
			f.cardAnswer = answerCard(option, 40*time.Second)
			out, logs := run(t, f, away(), nil, Claude, claudePermission)
			if out != want {
				t.Fatalf("stdout\n got  %s\n want %s\nlog:\n%s", out, want, logs)
			}
			create := f.sent("POST", "/activities")
			if len(create) != 1 || create[0].Body["name"] != "Claude Code" {
				t.Fatalf("creates %+v", create)
			}
			// Every prompt has a card of its own, and an ended card counts
			// toward the account's activity limit until it is deleted.
			if create[0].Body["ended_ttl"] != float64(60) || create[0].Body["dismissal_ttl"] != float64(0) {
				t.Errorf("ended_ttl %v, dismissal_ttl %v", create[0].Body["ended_ttl"], create[0].Body["dismissal_ttl"])
			}
			if f.probes != 1 {
				t.Errorf("%d key checks", f.probes)
			}
			slug, _ := create[0].Body["slug"].(string)
			if !regexp.MustCompile(`^agent-[0-9a-f]{8}-[0-9a-f]{8}$`).MatchString(slug) {
				t.Errorf("slug %q", slug)
			}
			steps := f.steps()
			if len(steps) != 1 {
				t.Fatalf("card updates %+v", steps)
			}
			c := steps[0]
			for k, v := range map[string]any{"template": "approval", "state": "Run a command", "source": "webapp", "on_expire": "none"} {
				if c[k] != v {
					t.Errorf("content.%s = %v, want %v", k, c[k], v)
				}
			}
			if c["step"] != nil || c["step_count"] != nil {
				t.Errorf("a one-question card has step fields: %v", c)
			}
			// Allow and Always ask for an unlock; Deny is one tap.
			wantOpts := `[{"authentication_required":true,"icon":"mdi:check","id":"allow","style":"primary","title":"Allow"},{"authentication_required":true,"icon":"mdi:check-all","id":"always","style":"secondary","title":"Always"},{"icon":"mdi:close","id":"deny","style":"destructive","title":"Deny"}]`
			if got := mustJSON(t, c["options"]); got != wantOpts {
				t.Errorf("options\n got  %s\n want %s", got, wantOpts)
			}
			// The command once, in the tool's row; the repo is the source.
			wantDetails := `[{"label":"Bash","value":"` + bashCommand + `"}]`
			if got := mustJSON(t, c["details"]); got != wantDetails {
				t.Errorf("details %s", got)
			}
			if end, _ := c["end_date"].(float64); int64(end) != time.Date(2000, 1, 1, 0, 25, 0, 0, time.UTC).Unix() {
				t.Errorf("end_date %v, want now + 25m", c["end_date"])
			}
			if len(f.notifs) != 0 || f.ended() {
				t.Errorf("a card answered in time needs no notification and no end: %d sent, ended %v", len(f.notifs), f.ended())
			}
		})
	}
}

// Without a permission suggestion there is nothing to apply for "always".
func TestClaudePermissionNoSuggestion(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = answerCard("allow", time.Second)
	in := strings.Replace(claudePermission, `,"permission_suggestions":[`+suggestion+`]`, "", 1)
	if out, _ := run(t, f, away(), nil, Claude, in); out != allowOut {
		t.Fatalf("stdout %s", out)
	}
	if got := mustJSON(t, f.steps()[0]["options"]); strings.Contains(got, "always") {
		t.Errorf("options %s", got)
	}
}

// A suggestion that lasts only for the session (Claude Code's setMode
// acceptEdits for a file edit) is offered as "This session", not "Always".
func TestClaudePermissionSessionSuggestion(t *testing.T) {
	const mode = `{"type":"setMode","mode":"acceptEdits","destination":"session"}`
	in := `{"session_id":"sess-1","cwd":"/home/u/src/webapp","hook_event_name":"PermissionRequest","tool_name":"Edit","tool_input":{"file_path":"/home/u/src/webapp/internal/app.go","old_string":"a","new_string":"b"},"permission_suggestions":[` + mode + `]}`
	f := newFakeAPI()
	f.cardAnswer = answerCard("always", time.Second)
	out, _ := run(t, f, away(), nil, Claude, in)
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[` + mode + `]}}}` + "\n"; out != want {
		t.Fatalf("stdout\n got  %s\n want %s", out, want)
	}
	c := f.steps()[0]
	if got := mustJSON(t, c["options"]); !strings.Contains(got, `{"authentication_required":true,"icon":"mdi:check-all","id":"always","style":"secondary","title":"This session"}`) {
		t.Errorf("options %s", got)
	}
	// A path with no drive letter is not absolute on Windows, so it shows
	// as given there.
	value := "internal/app.go"
	if runtime.GOOS == "windows" {
		value = "/home/u/src/webapp/internal/app.go"
	}
	if c["state"] != "Edit app.go" || mustJSON(t, c["details"]) != `[{"label":"Edit","value":"`+value+`"}]` {
		t.Errorf("state %v, details %v", c["state"], c["details"])
	}

	f = newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "deny", "", time.Second }
	o := away()
	o.Via = "notification"
	run(t, f, o, nil, Claude, in)
	if got := mustJSON(t, f.asked()[0]["actions"]); got != `[{"authentication_required":true,"id":"allow","title":"Allow"},{"authentication_required":true,"id":"always","title":"Allow for this session"},{"destructive":true,"id":"deny","title":"Deny"}]` {
		t.Errorf("actions %s", got)
	}
}

func TestCodexPermission(t *testing.T) {
	for option, want := range map[string]string{"allow": allowOut, "deny": denyOut} {
		t.Run(option, func(t *testing.T) {
			f := newFakeAPI()
			f.cardAnswer = answerCard(option, 3*time.Second)
			out, _ := run(t, f, away(), nil, Codex, codexPermission)
			if out != want {
				t.Fatalf("stdout\n got  %s\n want %s", out, want)
			}
			if strings.Contains(out, "updatedPermissions") || strings.Contains(out, "updatedInput") || strings.Contains(out, "interrupt") {
				t.Errorf("Codex fails closed on %s", out)
			}
			c := f.steps()[0]
			if c["state"] != "Run a command" || mustJSON(t, c["details"]) != `[{"label":"Bash","value":"cargo build --release"}]` {
				t.Errorf("state %v, details %v", c["state"], c["details"])
			}
			if got := mustJSON(t, c["options"]); got != `[{"authentication_required":true,"icon":"mdi:check","id":"allow","style":"primary","title":"Allow"},{"icon":"mdi:close","id":"deny","style":"destructive","title":"Deny"}]` {
				t.Errorf("Codex gets no Always: %s", got)
			}
			if name := f.sent("POST", "/activities")[0].Body["name"]; name != "Codex" {
				t.Errorf("name %v", name)
			}
		})
	}
}

// The plan review and Claude's questions never take the permission path. A
// plan waiting for review sends a notice while the user is away; at the
// desk, nothing.
func TestClaudePermissionSkipsQuestionsAndPlans(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		f := newFakeAPI()
		in := strings.Replace(claudePermission, `"tool_name":"Bash"`, `"tool_name":"`+tool+`"`, 1)
		out, _ := run(t, f, away(), nil, Claude, in)
		if out != "" || f.act != nil || len(f.asked()) != 0 {
			t.Errorf("%s: stdout %q, card %v, %d questions", tool, out, f.act != nil, len(f.asked()))
		}
		n := f.notices()
		switch {
		case tool == "AskUserQuestion" && len(f.calls) != 0:
			t.Errorf("%s: %d requests", tool, len(f.calls))
		case tool == "ExitPlanMode" && (len(n) != 1 || n[0]["title"] != "Claude Code needs you" || n[0]["body"] != "A plan is waiting for your review in the terminal"):
			t.Errorf("%s: notices %+v", tool, n)
		}
	}
	f := newFakeAPI()
	o := DefaultOptions()
	o.Desk = "desk"
	if run(t, f, o, nil, Claude, strings.Replace(claudePermission, `"tool_name":"Bash"`, `"tool_name":"ExitPlanMode"`, 1)); len(f.calls) != 0 {
		t.Errorf("a plan at the desk: %d requests", len(f.calls))
	}
}

// permissionPrompt is the Notification Claude Code sends 6 seconds into a
// dialog of session.
func permissionPrompt(session string) string {
	return `{"session_id":"` + session + `","hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`
}

// hookWith runs one Claude hook with dialogs as the Dialogs directory, inside
// the caller's synctest bubble.
func hookWith(f *fakeAPI, dialogs string, opts Options, idle Idler, stdin string) []byte {
	r, _ := newRunner(f, opts)
	r.Idle, r.Dialogs = idle, dialogs
	return r.Hook(context.Background(), Claude, strings.NewReader(stdin))
}

// Claude Code sends permission_prompt for the plan dialog 6 seconds after
// showing it; the plan's notice went out already, so that one sends nothing.
// Another session's prompt, the next prompt of the same session, and one a
// minute after a dialog's notice still notify.
func TestDialogNoticeIsNotRepeated(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "pushward", "agent-dialogs")
	plan := strings.Replace(claudePermission, `"tool_name":"Bash"`, `"tool_name":"ExitPlanMode"`, 1)
	multi := askInput(strings.Replace(questionsJSON, `"multiSelect":false}]`, `"multiSelect":true}]`, 1))
	f := newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		hook := func(stdin string, want int) {
			t.Helper()
			hookWith(f, dialogs, away(), nil, stdin)
			if n := len(f.notices()); n != want {
				t.Fatalf("%d notices, want %d: %+v", n, want, f.notices())
			}
		}
		hook(plan, 1)
		time.Sleep(6 * time.Second)
		hook(permissionPrompt("other"), 2)
		hook(permissionPrompt("sess-1"), 2)
		hook(permissionPrompt("sess-1"), 3)

		hook(multi, 4)
		time.Sleep(6 * time.Second)
		hook(permissionPrompt("sess-1"), 4)

		hook(plan, 5)
		time.Sleep(dialogFor)
		hook(permissionPrompt("sess-1"), 6)
	})
	if left, err := os.ReadDir(dialogs); err != nil || len(left) != 0 {
		t.Errorf("dialog files left: %v %v", left, err)
	}
}

// Claude Code sends permission_prompt 6 seconds into a permission dialog,
// while the PermissionRequest hook is still waiting on the phone.
func TestPromptOnThePhoneIsNotRepeated(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	f.cardAnswer = answerCard("allow", 40*time.Second)
	synctest.Test(t, func(t *testing.T) {
		var out []byte
		var wg sync.WaitGroup
		wg.Go(func() { out = hookWith(f, dialogs, away(), nil, claudePermission) })
		time.Sleep(6 * time.Second)
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		wg.Wait()
		if string(out) != allowOut {
			t.Errorf("stdout %s", out)
		}
		if n := len(f.notices()); n != 0 {
			t.Errorf("%d notices for a prompt already on the phone: %+v", n, f.notices())
		}
		// The next prompt left in the terminal is another dialog.
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		if n := len(f.notices()); n != 1 {
			t.Errorf("%d notices, want 1", n)
		}
	})
}

// Claude Code sends no permission_prompt for a prompt answered before its 6
// seconds are up, so the prompt that comes later is another dialog's.
func TestQuickAnswerDoesNotSwallowLaterPrompt(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	f.cardAnswer = answerCard("allow", 2*time.Second)
	synctest.Test(t, func(t *testing.T) {
		if out := hookWith(f, dialogs, away(), nil, claudePermission); string(out) != allowOut {
			t.Fatalf("stdout %s", out)
		}
		time.Sleep(10 * time.Second)
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		if n := len(f.notices()); n != 1 {
			t.Errorf("%d notices for a prompt left in the terminal, want 1", n)
		}
	})
}

// An answer around Claude Code's 6 seconds: a permission_prompt it sent just
// before the answer still finds the file, and a later one is not swallowed.
func TestAnsweredDialogStandsBriefly(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	f.cardAnswer = answerCard("allow", 5500*time.Millisecond)
	synctest.Test(t, func(t *testing.T) {
		for _, wait := range []time.Duration{time.Second, 10 * time.Second} {
			if out := hookWith(f, dialogs, away(), nil, claudePermission); string(out) != allowOut {
				t.Fatalf("stdout %s", out)
			}
			time.Sleep(wait)
			hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		}
		if n := len(f.notices()); n != 1 {
			t.Errorf("%d notices, want 1 for the prompt 10s after the answer: %+v", n, f.notices())
		}
	})
}

// The hook counts the user as away before it sends anything, so a card that
// takes longer than 6 seconds to go up is not pushed a second time.
func TestSlowCardIsNotRepeated(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	f.createDelay = 7 * time.Second
	f.cardAnswer = answerCard("allow", 40*time.Second)
	synctest.Test(t, func(t *testing.T) {
		var out []byte
		var wg sync.WaitGroup
		wg.Go(func() { out = hookWith(f, dialogs, away(), nil, claudePermission) })
		time.Sleep(6 * time.Second)
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		wg.Wait()
		if string(out) != allowOut || len(f.steps()) != 1 || len(f.notices()) != 0 {
			t.Errorf("stdout %s, %d card updates, notices %+v", out, len(f.steps()), f.notices())
		}
	})
}

// Two prompts of one session at once (parallel subagents): each has a file
// of its own, so neither permission_prompt pushes.
func TestConcurrentPromptsAreNotRepeated(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f1, f2, notified := newFakeAPI(), newFakeAPI(), newFakeAPI()
	f1.cardAnswer = answerCard("allow", 40*time.Second)
	f2.cardAnswer = answerCard("deny", 50*time.Second)
	second := strings.Replace(claudePermission, bashCommand, "make test", 1)
	synctest.Test(t, func(t *testing.T) {
		var out1, out2 []byte
		var wg sync.WaitGroup
		wg.Go(func() { out1 = hookWith(f1, dialogs, away(), nil, claudePermission) })
		wg.Go(func() { out2 = hookWith(f2, dialogs, away(), nil, second) })
		time.Sleep(6 * time.Second)
		wg.Go(func() { hookWith(notified, dialogs, away(), nil, permissionPrompt("sess-1")) })
		wg.Go(func() { hookWith(notified, dialogs, away(), nil, permissionPrompt("sess-1")) })
		wg.Wait()
		if string(out1) != allowOut || string(out2) != denyOut {
			t.Errorf("stdout %s %s", out1, out2)
		}
	})
	if len(f1.steps()) != 1 || len(f2.steps()) != 1 {
		t.Errorf("card updates %d %d, want a card each", len(f1.steps()), len(f2.steps()))
	}
	if n := len(f1.notices()) + len(f2.notices()) + len(notified.notices()); n != 0 {
		t.Errorf("%d notices", n)
	}
}

// Neither a card nor a notification goes out: the PermissionRequest hook
// sends the notice itself, and the permission_prompt that follows sends none.
func TestPromptLeftInTheTerminalNotifiesOnce(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	f.createStatus = http.StatusUnprocessableEntity
	f.askStatus = http.StatusTooManyRequests
	synctest.Test(t, func(t *testing.T) {
		if out := hookWith(f, dialogs, away(), nil, claudePermission); out != nil || len(f.notices()) != 1 {
			t.Errorf("stdout %s, notices %+v", out, f.notices())
		}
		time.Sleep(6 * time.Second)
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
	})
	if n := f.notices(); len(n) != 1 || n[0]["title"] != "Claude Code needs you" || n[0]["body"] != waitingText {
		t.Errorf("notices %+v", n)
	}
}

// A claude -p run has no one at its terminal, and Claude Code denies the
// prompt the hook leaves there, so nothing says it waits.
func TestUnattendedPromptGetsNoNotice(t *testing.T) {
	f := newFakeAPI()
	f.createStatus = http.StatusUnprocessableEntity
	f.askStatus = http.StatusTooManyRequests
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		r.Dialogs = filepath.Join(t.TempDir(), "agent-dialogs")
		r.Getenv = func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ATTENDED" {
				return "0"
			}
			return ""
		}
		if out := r.Hook(context.Background(), Claude, strings.NewReader(claudePermission)); out != nil || len(f.notices()) != 0 {
			t.Errorf("stdout %s, notices %+v", out, f.notices())
		}
	})
}

// A notice sent while Claude Code waits to show its dialog gives up after
// noticeWithin, and a notice that did not go out leaves no file.
func TestDialogNoticeDoesNotHoldTheDialog(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	plan := strings.Replace(claudePermission, `"tool_name":"Bash"`, `"tool_name":"ExitPlanMode"`, 1)
	f := newFakeAPI()
	f.noticeDelay = time.Minute
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		hookWith(f, dialogs, away(), nil, plan)
		if d := time.Since(start); d > noticeWithin {
			t.Errorf("the plan's hook took %s", d)
		}
	})
	if left, err := os.ReadDir(dialogs); len(left) != 0 {
		t.Errorf("dialog files %v %v", left, err)
	}
}

// A prompt that showed at the desk left no file: its permission_prompt
// notifies when the user has left meanwhile.
func TestPromptAtTheDeskThenAway(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	atDesk := true
	idle := func(context.Context) (time.Duration, error) {
		if atDesk {
			return time.Second, nil
		}
		return time.Hour, nil
	}
	synctest.Test(t, func(t *testing.T) {
		if out := hookWith(f, dialogs, DefaultOptions(), idle, claudePermission); out != nil || len(f.calls) != 0 {
			t.Errorf("at the desk: stdout %s, %d requests", out, len(f.calls))
		}
		atDesk = false
		time.Sleep(6 * time.Second)
		hookWith(f, dialogs, DefaultOptions(), idle, permissionPrompt("sess-1"))
	})
	if n := f.notices(); len(n) != 1 {
		t.Errorf("notices %+v", n)
	}
}

func TestQuestionsStepByStep(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = func(step int) (string, time.Duration) {
		return map[int]string{1: "q1o2", 2: "q2o1", 3: "q3o2"}[step], time.Duration(step) * 10 * time.Second
	}
	out, logs := run(t, f, away(), nil, Claude, askInput(questionsJSON))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"questions":` + questionsJSON +
		`,"answers":{"Notify the team?":"Email","Ship it today?":"Yes, tag and push it to production now","Which database should we use?":"SQLite"}}}}` + "\n"
	if out != want {
		t.Fatalf("stdout\n got  %s\n want %s\nlog:\n%s", out, want, logs)
	}
	if n := len(f.sent("POST", "/activities")); n != 1 {
		t.Errorf("%d creates, want one card for the whole ask", n)
	}
	steps := f.steps()
	if len(steps) != 3 {
		t.Fatalf("%d card updates, want 3", len(steps))
	}
	wantOpts := []string{
		`[{"icon":"mdi:numeric-1-circle","id":"q1o1","title":"Postgres"},{"icon":"mdi:numeric-2-circle","id":"q1o2","title":"SQLite"},{"icon":"mdi:numeric-3-circle","id":"q1o3","title":"DynamoDB"}]`,
		`[{"id":"q2o1","title":"Yes, tag and push it ..."},{"id":"q2o2","title":"No"}]`,
		`[{"id":"q3o1","title":"Slack"},{"id":"q3o2","title":"Email"}]`,
	}
	wantState := []string{"Which database should we use?", "Ship it today?", "Notify the team?"}
	for i, s := range steps {
		if s["step"] != float64(i+1) || s["step_count"] != float64(3) {
			t.Errorf("update %d: step %v of %v", i, s["step"], s["step_count"])
		}
		if s["state"] != wantState[i] {
			t.Errorf("update %d: state %v", i, s["state"])
		}
		if got := mustJSON(t, s["options"]); got != wantOpts[i] {
			t.Errorf("update %d options\n got  %s\n want %s", i, got, wantOpts[i])
		}
		// The question is the card's text alone: a row would show it on
		// one line.
		if s["details"] != nil {
			t.Errorf("update %d details %v", i, s["details"])
		}
	}
	if len(f.notifs) != 0 || f.ended() {
		t.Errorf("the card ends itself after the last answer: %d notifications, ended %v", len(f.notifs), f.ended())
	}
	if f.probes != 1 {
		t.Errorf("%d key checks for one ask", f.probes)
	}
}

// A server from before steps ends the card at the first answer: the other
// questions go out as notifications, and the ended card is left alone.
func TestServerWithoutSteps(t *testing.T) {
	f := newFakeAPI()
	f.noSteps = true
	f.cardAnswer = answerCard("q1o2", 5*time.Second)
	f.notifAnswer = func(n int, _ map[string]any) (string, string, time.Duration) {
		return map[int]string{1: "q2o1", 2: "q3o2"}[n], "", 3 * time.Second
	}
	out, logs := run(t, f, away(), nil, Claude, askInput(questionsJSON))
	if !strings.Contains(out, `"answers":{"Notify the team?":"Email","Ship it today?":"Yes, tag and push it to production now","Which database should we use?":"SQLite"}`) {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	if steps := f.steps(); len(steps) != 1 || steps[0]["step"] != float64(1) {
		t.Errorf("card updates %+v, want only the first question", steps)
	}
	asked := f.asked()
	if len(asked) != 2 || asked[0]["title"] != "Claude Code asks (2/3)" || asked[1]["title"] != "Claude Code asks (3/3)" {
		t.Errorf("notifications %+v", asked)
	}
	if f.ended() || len(f.retracted()) != 0 {
		t.Errorf("ended by the agent %v, retracted %v", f.ended(), f.retracted())
	}
}

// The first question's notification went out because no device reported
// the card in time, then the card was answered: it works, so the next
// questions go to the card alone.
func TestCardAnswerStopsTheFallback(t *testing.T) {
	f := newFakeAPI()
	f.started = 0
	f.cardAnswer = func(step int) (string, time.Duration) {
		after := 5 * time.Second
		if step == 1 {
			after = 20 * time.Second
		}
		return map[int]string{1: "q1o1", 2: "q2o2", 3: "q3o1"}[step], after
	}
	out, logs := run(t, f, away(), nil, Claude, askInput(questionsJSON))
	if !strings.Contains(out, `"answers":{"Notify the team?":"Slack","Ship it today?":"No","Which database should we use?":"Postgres"}`) {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	if asked := f.asked(); len(asked) != 1 || asked[0]["title"] != "Claude Code asks (1/3)" {
		t.Errorf("notifications %+v, want only the first question's", asked)
	}
	slug := f.sent("POST", "/activities")[0].Body["slug"]
	if r := f.retracted(); len(r) != 1 || r[0] != slug {
		t.Errorf("retracted %v, want the first question's notification", r)
	}
}

func TestSingleQuestionHasNoSteps(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = answerCard("q1o3", 5*time.Second)
	q := `[{"question":"Which database?","header":"DB","options":[{"label":"Postgres","description":""},{"label":"SQLite","description":""},{"label":"MySQL","description":""}],"multiSelect":false}]`
	out, _ := run(t, f, away(), nil, Claude, askInput(q))
	if !strings.Contains(out, `"answers":{"Which database?":"MySQL"}`) {
		t.Fatalf("stdout %s", out)
	}
	if s := f.steps()[0]; s["step"] != nil || s["step_count"] != nil {
		t.Errorf("step fields on a single question: %v", s)
	}
}

func TestMultiSelectGoesToTheTerminal(t *testing.T) {
	f := newFakeAPI()
	q := strings.Replace(questionsJSON, `"multiSelect":false}]`, `"multiSelect":true}]`, 1)
	out, _ := run(t, f, away(), nil, Claude, askInput(q))
	if out != "" {
		t.Fatalf("stdout %q", out)
	}
	if f.act != nil {
		t.Error("a card was created")
	}
	n := f.notices()
	if len(n) != 1 || n[0]["title"] != "Claude Code needs you" || n[0]["body"] != "Which database should we use?" {
		t.Fatalf("notices %+v", n)
	}
}

// No device starts the card within 15s: the question comes as a notification
// too, the notification's answer wins, and the card is ended. The answered
// notification is not retracted: the same call asked again gets its
// collapse id.
func TestFallbackNotificationWins(t *testing.T) {
	f := newFakeAPI()
	f.started = 0
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "deny", "", 20 * time.Second }
	out, logs := run(t, f, away(), nil, Claude, claudePermission)
	if out != denyOut {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	asked := f.asked()
	if len(asked) != 1 {
		t.Fatalf("%d notifications", len(asked))
	}
	n := asked[0]
	slug := f.sent("POST", "/activities")[0].Body["slug"]
	for k, v := range map[string]any{"title": "Claude Code asks", "body": "Run: " + bashCommand, "level": "time-sensitive", "collapse_id": slug, "thread_id": "pushward-agent", "subtitle": "webapp"} {
		if n[k] != v {
			t.Errorf("notification %s = %v, want %v", k, n[k], v)
		}
	}
	if got := mustJSON(t, n["actions"]); got != `[{"authentication_required":true,"id":"allow","title":"Allow"},{"authentication_required":true,"id":"always","title":"Always allow"},{"destructive":true,"id":"deny","title":"Deny"}]` {
		t.Errorf("actions %s", got)
	}
	if !f.ended() {
		t.Error("the card that lost was not ended")
	}
	if r := f.retracted(); len(r) != 0 {
		t.Errorf("the answered notification was retracted: %v", r)
	}
	// The notification went out once the start window passed.
	if at := f.notifs[0].at.Sub(f.act.created); at < 15*time.Second || at > 16*time.Second {
		t.Errorf("notification sent %s after the card", at)
	}
}

// The card is answered after the notification went out: the notification is
// retracted.
func TestFallbackCardWins(t *testing.T) {
	f := newFakeAPI()
	f.started = 0
	f.cardAnswer = answerCard("allow", 30*time.Second)
	out, _ := run(t, f, away(), nil, Claude, claudePermission)
	if out != allowOut {
		t.Fatalf("stdout %s", out)
	}
	if len(f.asked()) != 1 {
		t.Fatalf("no fallback notification")
	}
	slug := f.sent("POST", "/activities")[0].Body["slug"]
	if r := f.retracted(); len(r) != 1 || r[0] != slug {
		t.Errorf("retracted %v, want [%v]", r, slug)
	}
	if f.ended() {
		t.Error("the answered card is the server's to end")
	}
}

// Devices confirm the card: no notification at all.
func TestConfirmedCardSendsNoNotification(t *testing.T) {
	f := newFakeAPI()
	f.started = 1
	f.cardAnswer = answerCard("deny", 5*time.Minute)
	if out, _ := run(t, f, away(), nil, Claude, claudePermission); out != denyOut {
		t.Fatalf("stdout %s", out)
	}
	if len(f.notifs) != 0 {
		t.Errorf("%d notifications", len(f.notifs))
	}
}

// An account with no Apple device to start the card on gets the
// notification as soon as the card is up, not after the start window.
func TestNoDeviceToStartOnNotifiesAtOnce(t *testing.T) {
	f := newFakeAPI()
	f.started, f.targeted = 0, 0
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", 5 * time.Second }
	out, logs := run(t, f, away(), nil, Claude, claudePermission)
	if out != allowOut {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	if len(f.asked()) != 1 || !f.ended() {
		t.Fatalf("%d notifications, card ended %v", len(f.asked()), f.ended())
	}
	if at := f.notifs[0].at.Sub(f.act.created); at > time.Second {
		t.Errorf("notification sent %s after the card", at)
	}
	if !strings.Contains(logs, "no device to start the card on") {
		t.Errorf("log %s", logs)
	}
}

// The start check reads the card again when a read hangs or fails, a few
// times, before it gives up.
func TestStartCheckReadsAgain(t *testing.T) {
	for hangs, want := range map[int]bool{0: true, 2: true, 3: false} {
		f := newFakeAPI()
		f.act = &fakeActivity{state: "ongoing", content: map[string]any{}}
		f.started, f.hangReads = 0, hangs
		var sc startCount
		synctest.Test(t, func(t *testing.T) {
			r, _ := newRunner(f, away())
			sc = r.newAsk(Claude, Input{SessionID: "s", ToolName: "Bash"}).startCounts(context.Background())
		})
		if sc.ok != want || (want && (sc.targeted != 1 || sc.started != 0)) {
			t.Errorf("%d hung reads: %+v", hangs, sc)
		}
		if reads := len(f.sent("GET", "/activities/")); reads != min(hangs+1, startReads) {
			t.Errorf("%d hung reads: %d reads", hangs, reads)
		}
	}
	// A refusal that is not transient is not read again.
	f := newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		if sc := r.newAsk(Claude, Input{SessionID: "s", ToolName: "Bash"}).startCounts(context.Background()); sc.ok {
			t.Errorf("no card: %+v", sc)
		}
	})
	if reads := len(f.sent("GET", "/activities/")); reads != 1 {
		t.Errorf("a 404 was read %d times", reads)
	}
}

// A personal key refused the card (the plan, a key that may only send
// notifications) gets the notification: its own check says it is personal.
func TestCreateRefusedSendsNotificationOnly(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{{402, ""}, {403, ""}, {403, "integration_key.permission_denied"}, {422, ""}} {
		f := newFakeAPI()
		f.createStatus, f.createCode = c.status, c.code
		f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", time.Minute }
		if out, _ := run(t, f, away(), nil, Claude, claudePermission); out != allowOut {
			t.Fatalf("%d %s: stdout %s", c.status, c.code, out)
		}
		if len(f.steps()) != 0 || len(f.asked()) != 1 || f.probes != 2 {
			t.Errorf("%d %s: %d card updates, %d notifications, %d key checks", c.status, c.code, len(f.steps()), len(f.asked()), f.probes)
		}
	}
}

func TestViaNotification(t *testing.T) {
	f := newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", time.Second }
	o := away()
	o.Via = "notification"
	if out, _ := run(t, f, o, nil, Codex, codexPermission); out != allowOut {
		t.Fatalf("stdout %s", out)
	}
	if len(f.sent("POST", "/activities")) != 0 {
		t.Error("--via notification created a card")
	}
	if got := mustJSON(t, f.asked()[0]["actions"]); got != `[{"authentication_required":true,"id":"allow","title":"Allow"},{"destructive":true,"id":"deny","title":"Deny"}]` {
		t.Errorf("actions %s", got)
	}
}

// Every long-poll slot of the account is taken: reads go on without ?wait=,
// every 3s.
func TestWaitLimitFallsBackToPolling(t *testing.T) {
	f := newFakeAPI()
	f.waitLimit = true
	f.cardAnswer = answerCard("allow", 10*time.Second)
	if out, logs := run(t, f, away(), nil, Claude, claudePermission); out != allowOut {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	// One long-poll, refused; then plain reads every 3s until the answer at
	// 10s, and the start check's read right after the card started.
	reads := f.sent("GET", "/activities/")
	long, plain := 0, 0
	for _, r := range reads {
		switch r.Query.Get("wait") {
		case "":
			plain++
		case "25":
			long++
		}
	}
	if long != 1 || plain < 4 || plain > 6 {
		t.Errorf("%d reads, %d long-polls, %d plain", len(reads), long, plain)
	}
}

func TestNotificationAnswersWithTextAndStrayActions(t *testing.T) {
	f := newFakeAPI()
	f.notifAnswer = func(n int, sent map[string]any) (string, string, time.Duration) {
		// The first question gets a typed answer, the second an action of
		// another round, which is ignored, so the ask goes back to the
		// terminal.
		if n == 1 {
			return "q1other", "CockroachDB, multi-region", time.Second
		}
		return "q9o9", "", time.Second
	}
	o := away()
	o.Via = "notification"
	out, logs := run(t, f, o, nil, Claude, askInput(questionsJSON))
	if out != "" {
		t.Fatalf("stdout %s", out)
	}
	asked := f.asked()
	if len(asked) != 2 || asked[0]["title"] != "Claude Code asks (1/3)" || asked[1]["title"] != "Claude Code asks (2/3)" {
		t.Fatalf("notifications %+v", asked)
	}
	if got := asked[0]["body"]; got != "Which database should we use?\nPostgres: Relational, already in prod\nSQLite: One file\nDynamoDB: Managed" {
		t.Errorf("body %q", got)
	}
	if got := mustJSON(t, asked[0]["actions"]); !strings.HasSuffix(got, `{"id":"q1other","text_input":true,"text_input_placeholder":"Type an answer","title":"Other"}]`) {
		t.Errorf("actions %s", got)
	}
	if !strings.Contains(logs, "does not have") {
		t.Errorf("log %s", logs)
	}

	// A typed answer is returned as the text.
	f = newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) {
		return "q1other", "CockroachDB", time.Second
	}
	q := `[{"question":"Which database?","header":"DB","options":[{"label":"Postgres","description":""},{"label":"SQLite","description":""}],"multiSelect":false}]`
	out, _ = run(t, f, o, nil, Claude, askInput(q))
	if !strings.Contains(out, `"answers":{"Which database?":"CockroachDB"}`) {
		t.Errorf("stdout %s", out)
	}
}

// An option with a blank label is "Option N" on the card and in the
// notification, which the server would refuse with an empty action title.
func TestBlankOptionLabels(t *testing.T) {
	q := `[{"question":"Which one?","header":"Pick","options":[{"label":"  ","description":""},{"label":"Second","description":""},{"label":"","description":""}],"multiSelect":false}]`
	f := newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q1o2", "", time.Second }
	o := away()
	o.Via = "notification"
	out, _ := run(t, f, o, nil, Claude, askInput(q))
	if !strings.Contains(out, `"answers":{"Which one?":"Second"}`) {
		t.Fatalf("stdout %s", out)
	}
	if got := mustJSON(t, f.asked()[0]["actions"]); !strings.HasPrefix(got, `[{"id":"q1o1","title":"Option 1"},{"id":"q1o2","title":"Second"},{"id":"q1o3","title":"Option 3"},`) {
		t.Errorf("actions %s", got)
	}

	f = newFakeAPI()
	f.cardAnswer = answerCard("q1o1", time.Second)
	if out, _ := run(t, f, away(), nil, Claude, askInput(q)); !strings.Contains(out, `"answers":{"Which one?":"  "}`) {
		t.Errorf("stdout %s", out)
	}
	if got := mustJSON(t, f.steps()[0]["options"]); !strings.Contains(got, `"title":"Option 1"`) || !strings.Contains(got, `"title":"Option 3"`) {
		t.Errorf("card options %s", got)
	}
}

// A question with more options than a card has buttons goes out as a
// notification; the card carries the questions around it.
func TestQuestionTooWideForTheCard(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = func(step int) (string, time.Duration) { return fmt.Sprintf("q%do1", step), 2 * time.Second }
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q2o5", "", 2 * time.Second }
	q := `[{"question":"First?","header":"A","options":[{"label":"Yes","description":""},{"label":"No","description":""}],"multiSelect":false},` +
		`{"question":"Pick one","header":"B","options":[{"label":"a","description":""},{"label":"b","description":""},{"label":"c","description":""},{"label":"d","description":""},{"label":"e","description":""}],"multiSelect":false},` +
		`{"question":"Last?","header":"C","options":[{"label":"Yes","description":""},{"label":"No","description":""}],"multiSelect":false}]`
	out, logs := run(t, f, away(), nil, Claude, askInput(q))
	if !strings.Contains(out, `"answers":{"First?":"Yes","Last?":"Yes","Pick one":"e"}`) {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	steps := f.steps()
	if len(steps) != 2 || steps[0]["step"] != float64(1) || steps[1]["step"] != float64(3) {
		t.Errorf("card steps %+v", steps)
	}
	if len(f.asked()) != 1 || f.asked()[0]["title"] != "Claude Code asks (2/3)" {
		t.Errorf("notifications %+v", f.asked())
	}
}

// Options whose titles come out the same once cut to a card button (or
// differ only in case) cannot be told apart on the card: the question goes
// out as a notification, whose buttons have room for the labels. With --via
// card it stays in the terminal.
func TestSameCutTitlesSkipTheCard(t *testing.T) {
	for name, opts := range map[string]string{
		"cut":  `[{"label":"Deploy to production right now","description":""},{"label":"Deploy to production after lunch","description":""}]`,
		"case": `[{"label":"Yes","description":""},{"label":"yes","description":""}]`,
	} {
		q := `[{"question":"When?","header":"Deploy","options":` + opts + `,"multiSelect":false}]`
		f := newFakeAPI()
		f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q1o2", "", time.Second }
		out, logs := run(t, f, away(), nil, Claude, askInput(q))
		if !strings.Contains(out, `"answers":{"When?":`) || f.act != nil || len(f.asked()) != 1 {
			t.Fatalf("%s: stdout %s, card %v, %d notifications\nlog:\n%s", name, out, f.act != nil, len(f.asked()), logs)
		}
		titles := mustJSON(t, f.asked()[0]["actions"])
		if name == "cut" && !strings.Contains(titles, `"title":"Deploy to production right now"`) {
			t.Errorf("%s: actions %s", name, titles)
		}

		f = newFakeAPI()
		o := away()
		o.Via = "card"
		if out, _ := run(t, f, o, nil, Claude, askInput(q)); out != "" || f.act != nil || len(f.asked()) != 0 {
			t.Errorf("%s --via card: stdout %q, card %v, %d notifications", name, out, f.act != nil, len(f.asked()))
		}
	}
}

// Credentials in a command, a question or the last message are masked on the
// card and in the notifications; what goes back to the agent is as it came.
func TestSecretsAreMaskedOnTheWayOut(t *testing.T) {
	secret := "hunter" + "2-x9"
	in := strings.Replace(claudePermission, bashCommand, "mysql --password="+secret+" -e 'drop table users'", 1)
	f := newFakeAPI()
	f.started = 0
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", 20 * time.Second }
	if out, _ := run(t, f, away(), nil, Claude, in); out != allowOut {
		t.Fatalf("stdout %s", out)
	}
	if got := mustJSON(t, f.steps()[0]["details"]); got != `[{"label":"Bash","value":"mysql --password=*** -e 'drop table users'"}]` {
		t.Errorf("details %s", got)
	}
	if got := f.asked()[0]["body"]; got != "Run: mysql --password=*** -e 'drop table users'" {
		t.Errorf("notification body %v", got)
	}

	q := `[{"question":"Use API_KEY=` + secret + ` for prod?","header":"Key","options":[{"label":"Yes","description":"token: ` + secret + `"},{"label":"No","description":""}],"multiSelect":false}]`
	f = newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q1o1", "", time.Second }
	o := away()
	o.Via = "notification"
	out, _ := run(t, f, o, nil, Claude, askInput(q))
	if got := f.asked()[0]["body"]; got != "Use API_KEY=*** for prod?\nYes: token: ***" {
		t.Errorf("question body %q", got)
	}
	// The questions go back unmasked, the answer keyed by the real text.
	if !strings.Contains(out, `"answers":{"Use API_KEY=`+secret+` for prod?":"Yes"}`) || !strings.Contains(out, `"description":"token: `+secret+`"`) {
		t.Errorf("stdout %s", out)
	}
	f = newFakeAPI()
	run(t, f, away(), nil, Claude, askInput(q))
	if s := f.steps()[0]["state"]; s != "Use API_KEY=*** for prod?" {
		t.Errorf("card state %v", s)
	}

	// Option labels are masked on the card's buttons and the notification's;
	// the agent gets the label back as it was.
	label := "Use " + fakeGitHub
	q = `[{"question":"Which one?","header":"Key","options":[{"label":"` + label + `","description":""},{"label":"PASSWORD=` + secret + `","description":""}],"multiSelect":false}]`
	f = newFakeAPI()
	f.cardAnswer = answerCard("q1o1", time.Second)
	out, _ = run(t, f, away(), nil, Claude, askInput(q))
	if got := mustJSON(t, f.steps()[0]["options"]); got != `[{"id":"q1o1","title":"Use ***"},{"id":"q1o2","title":"PASSWORD=***"}]` {
		t.Errorf("card options %s", got)
	}
	if !strings.Contains(out, `"answers":{"Which one?":"`+label+`"}`) {
		t.Errorf("stdout %s", out)
	}
	f = newFakeAPI()
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q1o2", "", time.Second }
	out, _ = run(t, f, o, nil, Claude, askInput(q))
	if got := mustJSON(t, f.asked()[0]["actions"]); !strings.HasPrefix(got, `[{"id":"q1o1","title":"Use ***"},{"id":"q1o2","title":"PASSWORD=***"},`) {
		t.Errorf("notification actions %s", got)
	}
	if !strings.Contains(out, `"answers":{"Which one?":"PASSWORD=`+secret+`"}`) {
		t.Errorf("stdout %s", out)
	}

	f = newFakeAPI()
	run(t, f, away(), nil, Codex, `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"Set DB_PASSWORD=`+secret+` in .env"}`)
	if n := f.notices(); len(n) != 1 || n[0]["body"] != "Set DB_PASSWORD=*** in .env" {
		t.Errorf("notices %+v", n)
	}
}

// The card ends with no answer (swiped away, or a server that ends it after
// the first step): the question goes on as a notification.
func TestCardEndedFallsBackToNotification(t *testing.T) {
	f := newFakeAPI()
	f.endCard = 5 * time.Second
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", time.Second }
	if out, _ := run(t, f, away(), nil, Claude, claudePermission); out != allowOut {
		t.Fatalf("stdout %s", out)
	}
	if len(f.asked()) != 1 || f.ended() {
		t.Errorf("%d notifications, card ended by the agent %v", len(f.asked()), f.ended())
	}
}

// The card is created but its start is refused: it is deleted, not left on
// the account, and the prompt goes out as a notification.
func TestStartRefusedDeletesTheCard(t *testing.T) {
	f := newFakeAPI()
	f.startStatus = 429
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "deny", "", time.Second }
	out, logs := run(t, f, away(), nil, Claude, claudePermission)
	if out != denyOut {
		t.Fatalf("stdout %s\nlog:\n%s", out, logs)
	}
	slug := f.sent("POST", "/activities")[0].Body["slug"]
	if d := f.sent("DELETE", "/activities/"); len(d) != 1 || d[0].Path != "/activities/"+slug.(string) {
		t.Errorf("deletes %+v", d)
	}
	if f.act != nil || len(f.asked()) != 1 || f.ended() {
		t.Errorf("card left %v, %d notifications, ended %v", f.act != nil, len(f.asked()), f.ended())
	}
}

// An organization's key reaches other members: nothing is sent, and the
// prompt stays in the terminal.
func TestOrgKeySendsNothing(t *testing.T) {
	stop := `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"done"}`
	for name, c := range map[string]struct {
		agent   Agent
		in, out string
	}{
		"permission": {Claude, claudePermission, ""},
		"codex":      {Codex, codexPermission, ""},
		"questions":  {Claude, askInput(questionsJSON), ""},
		"multi":      {Claude, askInput(strings.Replace(questionsJSON, `"multiSelect":false}]`, `"multiSelect":true}]`, 1)), ""},
		"stop":       {Claude, stop, ""},
		"codex stop": {Codex, stop, "{}\n"},
	} {
		f := newFakeAPI()
		f.org = true
		f.cardAnswer = answerCard("allow", time.Second)
		out, logs := run(t, f, away(), nil, c.agent, c.in)
		if out != c.out || len(f.calls) != 0 || f.probes != 1 {
			t.Errorf("%s: stdout %q, %d requests, %d key checks", name, out, len(f.calls), f.probes)
		}
		if !strings.Contains(logs, "organization's key") {
			t.Errorf("%s: log %s", name, logs)
		}
	}
	f := newFakeAPI()
	f.org = true
	var err error
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		_, err = r.Test(context.Background(), "")
	})
	if err != ErrOrgKey || len(f.calls) != 0 {
		t.Errorf("test: %v, %d requests", err, len(f.calls))
	}
}

// An organization's key that may send notifications but not manage
// activities is refused the card before its target is read. The fallback
// notification would still reach the organization, so the notification
// check finds it out and nothing is sent.
func TestOrgKeyWithoutActivitiesSendsNothing(t *testing.T) {
	stop := `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"done"}`
	notice := `{"session_id":"s","hook_event_name":"Notification","message":"waiting","notification_type":"agent_needs_input"}`
	for name, c := range map[string]struct {
		agent   Agent
		in, out string
	}{
		"permission": {Claude, claudePermission, ""},
		"codex":      {Codex, codexPermission, ""},
		"questions":  {Claude, askInput(questionsJSON), ""},
		"stop":       {Claude, stop, ""},
		"notice":     {Claude, notice, ""},
		"codex stop": {Codex, stop, "{}\n"},
	} {
		f := newFakeAPI()
		f.org = true
		f.createStatus, f.createCode = http.StatusForbidden, "integration_key.permission_denied"
		f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "allow", "", time.Second }
		out, logs := run(t, f, away(), nil, c.agent, c.in)
		if out != c.out || len(f.calls) != 0 || f.probes != 2 {
			t.Errorf("%s: stdout %q, %d requests, %d key checks", name, out, len(f.calls), f.probes)
		}
		if !strings.Contains(logs, "organization's key") {
			t.Errorf("%s: log %s", name, logs)
		}
	}
	f := newFakeAPI()
	f.org = true
	f.createStatus, f.createCode = http.StatusForbidden, "integration_key.permission_denied"
	var err error
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		_, err = r.Test(context.Background(), "")
	})
	if err != ErrOrgKey || len(f.calls) != 0 {
		t.Errorf("test: %v, %d requests", err, len(f.calls))
	}
}

// Refused both creates before the target is read, the key may be an
// organization's: nothing is sent.
func TestUncheckedKeySendsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		agent Agent
		in    string
	}{
		"permission": {Claude, claudePermission},
		"codex":      {Codex, codexPermission},
		"questions":  {Claude, askInput(questionsJSON)},
		"stop":       {Claude, `{"session_id":"s","cwd":"/w","hook_event_name":"Stop","last_assistant_message":"done"}`},
	} {
		f := newFakeAPI()
		f.createStatus, f.createCode = http.StatusForbidden, "integration_key.permission_denied"
		f.notifStatus, f.notifCode = http.StatusForbidden, "integration_key.permission_denied"
		out, logs := run(t, f, away(), nil, c.agent, c.in)
		if (out != "" && out != "{}\n") || len(f.calls) != 0 || f.probes != 2 {
			t.Errorf("%s: stdout %q, %d requests, %d key checks", name, out, len(f.calls), f.probes)
		}
		if !strings.Contains(logs, "sending nothing") {
			t.Errorf("%s: log %s", name, logs)
		}
	}
	f := newFakeAPI()
	f.createStatus, f.createCode = http.StatusForbidden, "integration_key.permission_denied"
	f.notifStatus, f.notifCode = http.StatusForbidden, "integration_key.permission_denied"
	var err error
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		_, err = r.Test(context.Background(), "")
	})
	if !errors.Is(err, ErrKeyUnchecked) || len(f.calls) != 0 {
		t.Errorf("test: %v, %d requests", err, len(f.calls))
	}
}

// The same call in the same session again: the old card's answer must not
// answer the new prompt.
func TestStaleAnswerIsNotReused(t *testing.T) {
	f := newFakeAPI()
	f.existing = `{"template":"approval","options":[{"id":"allow","title":"Allow"},{"id":"deny","title":"Deny"}],"answer":{"option":"allow","by":"user","at":1}}`
	f.cardAnswer = answerCard("deny", 30*time.Second)
	if out, _ := run(t, f, away(), nil, Claude, claudePermission); out != denyOut {
		t.Fatalf("stdout %s", out)
	}
	if len(f.sent("DELETE", "/activities/")) != 1 || len(f.sent("POST", "/activities")) != 2 {
		t.Errorf("the stale card was not started over: %d deletes, %d creates", len(f.sent("DELETE", "/activities/")), len(f.sent("POST", "/activities")))
	}
}

func TestAtDeskSendsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		opts Options
		idle Idler
	}{
		"probe": {DefaultOptions(), func(context.Context) (time.Duration, error) { return 10 * time.Second, nil }},
		"flag":  {Options{MaxWait: DefaultMaxWait, OnTimeout: "none", DeskIdle: DefaultDeskIdle, Desk: "desk", Via: "auto", Notices: "away"}, nil},
	} {
		for _, in := range []string{claudePermission, askInput(questionsJSON), `{"session_id":"s","hook_event_name":"Stop","last_assistant_message":"done"}`} {
			f := newFakeAPI()
			if out, _ := run(t, f, c.opts, c.idle, Claude, in); out != "" || len(f.calls) != 0 {
				t.Errorf("%s: stdout %q, %d requests", name, out, len(f.calls))
			}
		}
	}
}

// Input at the Mac while the prompt waits on the phone hands it back.
func TestInputMidWaitHandsBack(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = answerCard("allow", 10*time.Minute)
	// Away for the first 30s of the wait, then a key press.
	var start time.Time
	idle := func(context.Context) (time.Duration, error) {
		if start.IsZero() {
			start = time.Now()
		}
		if time.Since(start) < 30*time.Second {
			return 5 * time.Minute, nil
		}
		return time.Second, nil
	}
	// The user is back at the prompt: no notice, even with --notices always.
	o := DefaultOptions()
	o.Notices = "always"
	out, logs := run(t, f, o, idle, Claude, claudePermission)
	if out != "" {
		t.Fatalf("stdout %s", out)
	}
	if !f.ended() || !strings.Contains(logs, "input at the desk") || len(f.notices()) != 0 {
		t.Errorf("card ended %v, notices %+v; log:\n%s", f.ended(), f.notices(), logs)
	}
}

func TestMaxWait(t *testing.T) {
	for onTimeout, want := range map[string]string{
		"none": "",
		"deny": `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"No answer from PushWard in time"}}}` + "\n",
	} {
		f := newFakeAPI()
		o := away()
		o.OnTimeout = onTimeout
		o.MaxWait = 3 * time.Minute
		if out, _ := run(t, f, o, nil, Claude, claudePermission); out != want {
			t.Errorf("%s: stdout %q", onTimeout, out)
		}
		if !f.ended() {
			t.Errorf("%s: the card was left up", onTimeout)
		}
		// A prompt left in the terminal gets a notice; a denied one does not.
		if n := f.notices(); (onTimeout == "none") != (len(n) == 1 && n[0]["body"] == waitingText) || len(n) > 1 {
			t.Errorf("%s: notices %+v", onTimeout, n)
		}
	}
	// A question times out into a PreToolUse deny.
	f := newFakeAPI()
	o := away()
	o.OnTimeout, o.MaxWait = "deny", time.Minute
	out, _ := run(t, f, o, nil, Claude, askInput(questionsJSON))
	if out != `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"No answer from PushWard in time"}}`+"\n" {
		t.Errorf("questions: stdout %q", out)
	}
}

// The agent cancels the hook: the card is ended on the way out.
func TestCancelledCleansUp(t *testing.T) {
	f := newFakeAPI()
	f.started = 0
	var out string
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		out = string(r.Hook(ctx, Claude, strings.NewReader(claudePermission)))
	})
	if out != "" || !f.ended() || len(f.retracted()) != 1 {
		t.Errorf("stdout %q, ended %v, retracted %v", out, f.ended(), f.retracted())
	}
}

// Esc ends the turn before Claude Code's 6 seconds, so no permission_prompt
// follows: the session's next one still notifies.
func TestCancelledDoesNotSwallowLaterPrompt(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		r.Dialogs = dialogs
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		r.Hook(ctx, Claude, strings.NewReader(claudePermission))
		time.Sleep(10 * time.Second)
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		if n := len(f.notices()); n != 1 {
			t.Errorf("%d notices, want 1", n)
		}
	})
}

func TestErrorsPrintNothing(t *testing.T) {
	for name, status := range map[string]int{"401": http.StatusUnauthorized, "500": http.StatusInternalServerError} {
		f := newFakeAPI()
		f.status = status
		if out, _ := run(t, f, away(), nil, Claude, claudePermission); out != "" {
			t.Errorf("%s: stdout %q", name, out)
		}
	}
	// The network is down.
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(downTransport{}, away())
		if out := r.Hook(context.Background(), Claude, strings.NewReader(claudePermission)); out != nil {
			t.Errorf("network down: stdout %q", out)
		}
	})
	// Not JSON.
	r, _ := newRunner(newFakeAPI(), away())
	if out := r.Hook(context.Background(), Claude, strings.NewReader("not json")); out != nil {
		t.Errorf("bad input: stdout %q", out)
	}
}

func TestNotices(t *testing.T) {
	stop := `{"session_id":"sess-1","cwd":"/home/u/src/webapp","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"` + strings.Repeat("All tests pass. ", 20) + `"}`
	f := newFakeAPI()
	if out, _ := run(t, f, away(), nil, Claude, stop); out != "" {
		t.Errorf("Claude Stop prints %q", out)
	}
	n := f.notices()
	if len(n) != 1 {
		t.Fatalf("notices %+v", n)
	}
	body, _ := n[0]["body"].(string)
	if n[0]["title"] != "Claude Code finished" || len([]rune(body)) != 200 || !strings.HasSuffix(body, "...") || n[0]["level"] != "active" || n[0]["subtitle"] != "webapp" {
		t.Errorf("notice %+v", n[0])
	}
	if n[0]["collapse_id"] != "agent-"+hex8("sess-1") {
		t.Errorf("collapse_id %v", n[0]["collapse_id"])
	}

	// Codex wants {} from Stop; its message may be null.
	f = newFakeAPI()
	out, _ := run(t, f, away(), nil, Codex, `{"session_id":"cx-1","turn_id":"t","cwd":"/home/u/api","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":null}`)
	if out != "{}\n" || len(f.notices()) != 1 || f.notices()[0]["title"] != "Codex finished" || f.notices()[0]["body"] != "Done" {
		t.Errorf("Codex Stop: %q %+v", out, f.notices())
	}

	// idle_prompt comes a minute after Stop, which already sent a notice
	// with the same collapse id; an older install still sends it. A prompt
	// left in the terminal (handed back, or timed out) gets one while away.
	for typ, want := range map[string]int{"idle_prompt": 0, "agent_needs_input": 1, "permission_prompt": 1, "auth_success": 0} {
		f = newFakeAPI()
		run(t, f, away(), nil, Claude, `{"session_id":"s","hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"`+typ+`"}`)
		if got := len(f.notices()); got != want {
			t.Errorf("%s: %d notices", typ, got)
		} else if want == 1 && (f.notices()[0]["title"] != "Claude Code needs you" || f.notices()[0]["body"] != "Claude needs your permission to use Bash") {
			t.Errorf("%s: %+v", typ, f.notices())
		}
	}
	f = newFakeAPI()
	run(t, f, away(), nil, Claude, `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt"}`)
	if n := f.notices(); len(n) != 1 || n[0]["body"] != "A prompt is waiting in the terminal" {
		t.Errorf("permission_prompt with no message: %+v", n)
	}
	// A prompt at the desk is in front of the user, even with --notices
	// always; --notices off sends nothing away from it either.
	for _, c := range []struct {
		desk, notices string
		typ           string
		want          int
	}{
		{"desk", "always", "permission_prompt", 0},
		{"desk", "always", "agent_needs_input", 1},
		{"desk", "away", "permission_prompt", 0},
		{"away", "off", "permission_prompt", 0},
		{"away", "always", "permission_prompt", 1},
	} {
		f = newFakeAPI()
		o := DefaultOptions()
		o.Desk, o.Notices = c.desk, c.notices
		run(t, f, o, nil, Claude, `{"session_id":"s","hook_event_name":"Notification","message":"m","notification_type":"`+c.typ+`"}`)
		if got := len(f.notices()); got != c.want {
			t.Errorf("%s --desk %s --notices %s: %d notices", c.typ, c.desk, c.notices, got)
		}
	}

	// At the desk, or with notices off, nothing; always sends at the desk.
	for notices, want := range map[string]int{"away": 0, "off": 0, "always": 1} {
		f = newFakeAPI()
		o := DefaultOptions()
		o.Desk, o.Notices = "desk", notices
		run(t, f, o, nil, Claude, stop)
		if got := len(f.notices()); got != want {
			t.Errorf("--notices %s at the desk: %d notices", notices, got)
		}
	}
}

const stopInput = `{"session_id":"sess-1","cwd":"/home/u/src/webapp","hook_event_name":"Stop","last_assistant_message":"done"}`

// atTheDesk is the options of a user at the desk.
func atTheDesk() Options {
	o := DefaultOptions()
	o.Desk = "desk"
	return o
}

// A notice sent while away is taken back once, when a turn ends with the
// user at the desk; a turn that ends away replaces it instead.
func TestNoticeTakenBackAtTheDesk(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	id := "agent-" + hex8("sess-1")
	marker := filepath.Join(filepath.Dir(dialogs), "agent-notices", id)
	marked := func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
	f := newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		hookWith(f, dialogs, atTheDesk(), nil, stopInput)
		if len(f.calls) != 0 || marked() {
			t.Fatalf("no notice yet: %d requests, marker %v", len(f.calls), marked())
		}

		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		if len(f.notices()) != 1 || !marked() {
			t.Fatalf("away: %d notices, marker %v", len(f.notices()), marked())
		}
		hookWith(f, dialogs, away(), nil, stopInput)
		if len(f.notices()) != 2 || len(f.retracted()) != 0 || !marked() {
			t.Fatalf("Stop away: %d notices, retracted %v, marker %v", len(f.notices()), f.retracted(), marked())
		}

		hookWith(f, dialogs, atTheDesk(), nil, stopInput)
		if r := f.retracted(); len(r) != 1 || r[0] != id || marked() {
			t.Fatalf("Stop at the desk: retracted %v, want [%s]; marker %v", r, id, marked())
		}
		hookWith(f, dialogs, atTheDesk(), nil, stopInput)
		if r := f.retracted(); len(r) != 1 {
			t.Fatalf("second Stop at the desk: retracted %v", r)
		}

		// --notices off takes nothing back; always replaces at the desk.
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		off := atTheDesk()
		off.Notices = "off"
		hookWith(f, dialogs, off, nil, stopInput)
		always := atTheDesk()
		always.Notices = "always"
		hookWith(f, dialogs, always, nil, stopInput)
		if len(f.notices()) != 4 || len(f.retracted()) != 1 || !marked() {
			t.Fatalf("off, then always: %d notices, retracted %v, marker %v", len(f.notices()), f.retracted(), marked())
		}

		// A notice removes the markers older than a day, of any session.
		stale := filepath.Join(filepath.Dir(marker), "agent-"+hex8("old"))
		recent := filepath.Join(filepath.Dir(marker), "agent-"+hex8("recent"))
		for path, age := range map[string]time.Duration{stale: noticeFor, recent: time.Hour} {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-age)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
		hookWith(f, dialogs, away(), nil, permissionPrompt("other"))
		if _, err := os.Stat(stale); err == nil {
			t.Error("a day-old marker was kept")
		}
		if _, err := os.Stat(recent); err != nil || !marked() {
			t.Errorf("a fresh marker was removed: %v, marker %v", err, marked())
		}
	})
}

// Two turns that end at the desk at the same time take the notice back once.
func TestNoticeTakenBackOnce(t *testing.T) {
	dialogs := filepath.Join(t.TempDir(), "agent-dialogs")
	f := newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		hookWith(f, dialogs, away(), nil, permissionPrompt("sess-1"))
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { hookWith(f, dialogs, atTheDesk(), nil, stopInput) })
		}
		wg.Wait()
	})
	if r := f.retracted(); len(r) != 1 {
		t.Errorf("retracted %v, want one", r)
	}
}

// TestAgentNeverLogsTheCommand runs every flow with distinctive text in the
// command, the typed answer, the answer URLs, the key and a server error
// that quotes the command back, and checks none of it reaches the log.
func TestAgentNeverLogsTheCommand(t *testing.T) {
	const typed = "TYPED-ANSWER-7731"
	const answerURL = "https://api.pushward.test/activity/answer?token=SIGNED-TOKEN-4410"
	secrets := []string{bashCommand, "prod-eu-7f3a", "cargo build", typed, "SIGNED-TOKEN", testToken, "Which database", "Postgres", "All tests pass"}
	var logs []string
	check := func(f *fakeAPI, o Options, idle Idler, agent Agent, in string) {
		_, l := run(t, f, o, idle, agent, in)
		logs = append(logs, l)
	}

	f := newFakeAPI()
	f.started, f.optionURL = 0, answerURL
	f.cardAnswer = answerCard("always", 20*time.Second)
	check(f, away(), nil, Claude, claudePermission)

	f = newFakeAPI()
	f.createStatus, f.createDetail = 422, "content.state: got "+bashCommand
	f.notifAnswer = func(int, map[string]any) (string, string, time.Duration) { return "q1other", typed, time.Second }
	check(f, away(), nil, Claude, askInput(questionsJSON))

	f = newFakeAPI()
	f.waitLimit, f.optionURL = true, answerURL
	f.cardAnswer = func(step int) (string, time.Duration) { return fmt.Sprintf("q%do1", step), 5 * time.Second }
	check(f, away(), nil, Claude, askInput(questionsJSON))

	o := DefaultOptions()
	o.MaxWait = time.Minute
	check(newFakeAPI(), o, func(context.Context) (time.Duration, error) { return time.Hour, nil }, Codex, codexPermission)
	check(newFakeAPI(), away(), nil, Claude, `{"session_id":"s","cwd":"/x","hook_event_name":"Stop","last_assistant_message":"All tests pass"}`)

	all := strings.Join(logs, "\n")
	if all == "" {
		t.Fatal("nothing was logged at all")
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("the log holds %q:\n%s", s, all)
		}
	}
}

func TestTestCommandFlow(t *testing.T) {
	f := newFakeAPI()
	f.cardAnswer = func(step int) (string, time.Duration) { return fmt.Sprintf("q%do2", step), time.Second }
	var answers []Answer
	var err error
	synctest.Test(t, func(t *testing.T) {
		r, _ := newRunner(f, away())
		answers, err = r.Test(context.Background(), "/home/u/demo")
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"question":"Which database should the demo use?","answer":"SQLite"},{"question":"Run the migrations now?","answer":"Not yet"},{"question":"How should the release go out?","answer":"Draft only"}]`
	if got := mustJSON(t, answers); got != want {
		t.Errorf("answers %s", got)
	}
	if s := f.steps(); len(s) != 3 || s[2]["step"] != float64(3) || s[0]["source"] != "demo" {
		t.Errorf("steps %+v", s)
	}

	f = newFakeAPI()
	synctest.Test(t, func(t *testing.T) {
		o := away()
		o.MaxWait = time.Minute
		r, _ := newRunner(f, o)
		_, err = r.Test(context.Background(), "")
	})
	if err != ErrNoAnswer {
		t.Errorf("unanswered test: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
