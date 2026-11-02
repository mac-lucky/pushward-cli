package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// claudeSettings is a settings file with hooks of its own: the glasses hub
// on every event, and an auto-approve for MCP tools on PermissionRequest.
const claudeSettings = `{
  "model": "opus",
  "permissions": {
    "allow": [
      "Bash(go test:*)"
    ],
    "defaultMode": "default"
  },
  "hooks": {
    "PermissionRequest": [
      {
        "matcher": "mcp__.*",
        "hooks": [
          {
            "type": "command",
            "command": "echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\"}}}'"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "~/glasses/hooks/claude-code-event.sh",
            "async": true
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "~/glasses/hooks/claude-code-event.sh <stop> & done",
            "async": true
          }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "~/.claude/statusline.sh"
  }
}
`

const claudeCommand = "/opt/homebrew/bin/pushward agent hook claude"

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func install(t *testing.T, path string, agent Agent, command string) Plan {
	t.Helper()
	p, err := PlanInstall(path, agent, command, DefaultMaxWait)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallClaude(t *testing.T) {
	path := writeSettings(t, claudeSettings)
	p := install(t, path, Claude, claudeCommand)
	if !p.Changed() {
		t.Fatal("install changed nothing")
	}
	got := read(t, path)
	if bak := read(t, path+".bak"); bak != claudeSettings {
		t.Errorf("backup is not the file as it was:\n%s", bak)
	}
	// Our entries go after the hooks already there, and a new event after
	// the events already there; every other key keeps its place.
	want := strings.NewReplacer(
		`            "command": "~/glasses/hooks/claude-code-event.sh",
            "async": true
          }
        ]
      }
    ],`, `            "command": "~/glasses/hooks/claude-code-event.sh",
            "async": true
          }
        ]
      },
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/pushward agent hook claude",
            "timeout": 1560
          }
        ]
      }
    ],`,
		`            "command": "~/glasses/hooks/claude-code-event.sh <stop> & done",
            "async": true
          }
        ]
      }
    ]
  },`, `            "command": "~/glasses/hooks/claude-code-event.sh <stop> & done",
            "async": true
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/pushward agent hook claude",
            "async": true
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "AskUserQuestion",
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/pushward agent hook claude",
            "timeout": 1560
          }
        ]
      }
    ],
    "Notification": [
      {
        "matcher": "idle_prompt|agent_needs_input",
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/pushward agent hook claude",
            "async": true
          }
        ]
      }
    ]
  },`).Replace(claudeSettings)
	if got != want {
		t.Errorf("settings after install:\n%s\nwant:\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
			t.Errorf("mode %v, want the file's own 0644", info.Mode().Perm())
		}
	}

	// Again: nothing changes, and the backup of the original stays.
	if p := install(t, path, Claude, claudeCommand); p.Changed() {
		t.Errorf("a second install changed the file:\n%s", p.After)
	}
	if read(t, path) != want || read(t, path+".bak") != claudeSettings {
		t.Error("a second install rewrote the file or its backup")
	}

	events, err := Installed(path, Claude)
	if err != nil || strings.Join(events, ",") != "PermissionRequest,Stop,PreToolUse,Notification" {
		t.Errorf("installed %v %v", events, err)
	}
	if events, _ := Installed(path, Codex); len(events) != 0 {
		t.Errorf("codex installed %v", events)
	}

	// A moved binary: the entry is updated in place, not added twice.
	install(t, path, Claude, "/usr/local/bin/pushward agent hook claude")
	if got := read(t, path); strings.Count(got, "agent hook claude") != 4 || strings.Contains(got, "/opt/homebrew") {
		t.Errorf("after a reinstall from another path:\n%s", got)
	}

	// Uninstall leaves the file as it was before the install.
	p, err = PlanUninstall(path, Claude)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != claudeSettings {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", got, claudeSettings)
	}
	if p, _ := PlanUninstall(path, Claude); p.Changed() {
		t.Error("a second uninstall changed the file")
	}
}

func TestInstallCodex(t *testing.T) {
	const hooks = `{
  "description": "my hooks",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "python3 ~/.codex/policy.py",
            "timeout": 30
          }
        ]
      }
    ]
  }
}
`
	path := writeSettings(t, hooks)
	command := "/home/linuxbrew/.linuxbrew/bin/pushward agent hook codex"
	p, err := PlanInstall(path, Codex, command, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(hooks, `    ]
  }
}`, `    ],
    "PermissionRequest": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/home/linuxbrew/.linuxbrew/bin/pushward agent hook codex",
            "timeout": 2460
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/home/linuxbrew/.linuxbrew/bin/pushward agent hook codex",
            "timeout": 60,
            "async": true
          }
        ]
      }
    ]
  }
}`, 1)
	if got := read(t, path); got != want {
		t.Errorf("hooks.json after install:\n%s\nwant:\n%s", got, want)
	}
	// Claude's uninstall leaves Codex's hooks alone.
	if p, _ := PlanUninstall(path, Claude); p.Changed() {
		t.Error("uninstalling claude touched codex's hooks")
	}
	p, err = PlanUninstall(path, Codex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != hooks {
		t.Errorf("after uninstall:\n%s", got)
	}
}

func TestInstallCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "hooks.json")
	p := install(t, path, Codex, "pushward agent hook codex")
	if p.Before != nil || !p.Changed() {
		t.Fatalf("plan %+v", p)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a backup of a file that did not exist")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", info.Mode().Perm())
		}
	}
	p, _ = PlanUninstall(path, Codex)
	if string(p.After) != "{}\n" {
		t.Errorf("after uninstall %q", p.After)
	}
	// Nothing to take out of a file that is not there.
	if p, err := PlanUninstall(filepath.Join(t.TempDir(), "none.json"), Codex); err != nil || p.Changed() {
		t.Errorf("uninstall of a missing file: %+v %v", p, err)
	}
}

// A settings file linked from a dotfiles repository stays a link.
func TestInstallFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	target := writeSettings(t, "{}\n")
	link := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	install(t, link, Claude, claudeCommand)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if !strings.Contains(read(t, target), "agent hook claude") {
		t.Error("the link's target has no hooks")
	}
}

func TestInstallRefusesWhatItCannotMerge(t *testing.T) {
	for _, content := range []string{`[1, 2]`, `{"hooks": []}`, `{"hooks": {"Stop": {}}}`, `{"a": 1} trailing`} {
		path := writeSettings(t, content)
		if _, err := PlanInstall(path, Claude, claudeCommand, DefaultMaxWait); err == nil {
			t.Errorf("%s: merged", content)
		}
		if read(t, path) != content {
			t.Errorf("%s: file changed", content)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/homebrew/bin/pushward":     "/opt/homebrew/bin/pushward",
		"/Users/a b/bin/pushward":        "'/Users/a b/bin/pushward'",
		"/tmp/it's/pushward":             `'/tmp/it'\''s/pushward'`,
		"/home/u/go/bin/pushward-v1.8.0": "/home/u/go/bin/pushward-v1.8.0",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "pushward", "1.8.0", "bin", "pushward")
	if err := os.MkdirAll(filepath.Dir(keg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keg, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	if got := stablePath(keg); got != keg {
		t.Errorf("no bin link: %s", got)
	}
	link := filepath.Join(prefix, "bin", "pushward")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keg, link); err != nil {
		t.Fatal(err)
	}
	if got := stablePath(keg); got != link {
		t.Errorf("stablePath = %s, want the bin link %s", got, link)
	}
}
