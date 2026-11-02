package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, c := range []struct {
		tool, input string
		state       string
		row         string
	}{
		{"Bash", `{"command":"go test ./...\n  -race"}`, "Run: go test ./... -race", `{"label":"Bash","value":"go test ./... -race"}`},
		{"Bash", `{"command":["bash","-lc","make release"]}`, "Run: make release", `{"label":"Bash","value":"make release"}`},
		{"Edit", `{"file_path":"/home/u/src/webapp/internal/app.go","old_string":"a","new_string":"b"}`, "Edit internal/app.go", `{"label":"Edit","value":"internal/app.go"}`},
		{"Write", `{"file_path":"/etc/hosts","content":"secret"}`, "Edit /etc/hosts", `{"label":"Write","value":"/etc/hosts"}`},
		{"apply_patch", `{"command":"*** Begin Patch\n*** Update File: cmd/main.go\n@@\n-a\n+b\n*** Add File: docs/x.md\n+hi\n*** End Patch"}`, "Edit cmd/main.go and 1 more file", `{"label":"apply_patch","value":"cmd/main.go"}`},
		{"WebFetch", `{"url":"https://example.com/a","prompt":"read"}`, "WebFetch", `{"label":"WebFetch","value":"https://example.com/a"}`},
		{"mcp__github__create_pull_request", `{"owner":"me","repo":"r"}`, "mcp__github__create_pull_request", `{"label":"mcp__github__create_p...","value":"{\"owner\":\"me\",\"repo\":\"r\"}"}`},
		{"Bash", `{"command":"` + long + `"}`, "Run: " + strings.Repeat("x", 248) + "...", `{"label":"Bash","value":"` + strings.Repeat("x", 61) + `..."}`},
		{"ExitWorktree", `{}`, "ExitWorktree", "null"},
	} {
		in := Input{Cwd: "/home/u/src/webapp", ToolName: c.tool, ToolInput: json.RawMessage(c.input)}
		s := in.summarize()
		row, _ := json.Marshal(s.row)
		if s.state != c.state || string(row) != c.row {
			t.Errorf("%s %s:\n got  %q %s\n want %q %s", c.tool, c.input, s.state, row, c.state, c.row)
		}
	}
}

func TestSlug(t *testing.T) {
	a := Input{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls","description":"List"}`)}
	b := Input{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{ "description": "List", "command": "ls" }`)}
	c := Input{SessionID: "s2", ToolName: "Bash", ToolInput: a.ToolInput}
	if a.slug() != b.slug() {
		t.Errorf("key order or spacing changed the slug: %s %s", a.slug(), b.slug())
	}
	if a.slug() == c.slug() || !strings.HasPrefix(a.slug(), "agent-"+hex8("s1")+"-") || len(a.slug()) != 23 {
		t.Errorf("slugs %s %s", a.slug(), c.slug())
	}
	if a.noticeID() != "agent-"+hex8("s1") {
		t.Errorf("notice id %s", a.noticeID())
	}
}

func TestCut(t *testing.T) {
	for in, want := range map[string]string{
		"short":       "short",
		"exactly ten": "exactly...",
		// Runes, not bytes.
		"\u017c\u00f3\u0142\u0107\u017c\u00f3\u0142\u0107\u017c\u00f3\u0142\u0107": "\u017c\u00f3\u0142\u0107\u017c\u00f3\u0142...",
	} {
		if got := cut(in, 10); got != want {
			t.Errorf("cut(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.Via = "email" },
		func(o *Options) { o.Desk = "home" },
		func(o *Options) { o.OnTimeout = "allow" },
		func(o *Options) { o.Notices = "never" },
		func(o *Options) { o.MaxWait = 0 },
		func(o *Options) { o.DeskIdle = -time.Second },
	} {
		o := DefaultOptions()
		change(&o)
		if o.Validate() == nil {
			t.Errorf("%+v passed", o)
		}
	}
}

func TestParseHIDIdle(t *testing.T) {
	out := []byte(`+-o IOHIDSystem  <class IOHIDSystem, id 0x100000abc, registered, matched, active, busy 0 (0 ms), retain 30>
    {
      "HIDIdleTime" = 93125000000
      "IOClass" = "IOHIDSystem"
    }`)
	if d, err := parseHIDIdle(out); err != nil || d != 93125*time.Millisecond {
		t.Errorf("parseHIDIdle = %s, %v", d, err)
	}
	if _, err := parseHIDIdle([]byte("nothing here")); err == nil {
		t.Error("no HIDIdleTime parsed")
	}
}
