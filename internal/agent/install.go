package agent

// Installing the hooks: entries merged into Claude Code's settings.json or
// Codex's hooks.json, next to whatever hooks are there already. Our entries
// are the ones whose command runs "agent hook <agent>"; uninstall takes out
// those and nothing else. One whose command the user wrapped in another
// (op run, env) is left as it is by install.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultSettings is the file the agent reads its hooks from:
// ~/.claude/settings.json ($CLAUDE_CONFIG_DIR) or ~/.codex/hooks.json
// ($CODEX_HOME).
func DefaultSettings(agent Agent) (string, error) {
	dirEnv, dir, file := "CLAUDE_CONFIG_DIR", ".claude", "settings.json"
	if agent == Codex {
		dirEnv, dir, file = "CODEX_HOME", ".codex", "hooks.json"
	}
	if d := os.Getenv(dirEnv); d != "" {
		return filepath.Join(d, file), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dir, file), nil
}

// HookCommand is the command line the hooks run: this binary, by a path that
// survives an upgrade, then "agent hook <agent>". Codex trusts a hook by its
// command, so the line has to come out the same on every install.
func HookCommand(agent Agent, maxWait time.Duration) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := shellQuote(stablePath(exe)) + " agent hook " + string(agent)
	if maxWait != DefaultMaxWait {
		cmd += " --max-wait " + maxWait.String()
	}
	return cmd, nil
}

// stablePath prefers the pushward on PATH when it is this binary, and
// Homebrew's bin link over the versioned Cellar path it points at.
func stablePath(exe string) string {
	if p, err := exec.LookPath("pushward"); err == nil {
		if abs, err := filepath.Abs(p); err == nil && sameFile(abs, exe) {
			return abs
		}
	}
	if i := strings.Index(exe, "/Cellar/"); i >= 0 {
		link := filepath.Join(exe[:i], "bin", filepath.Base(exe))
		if sameFile(link, exe) {
			return link
		}
	}
	return exe
}

func sameFile(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./@%+=:,-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// entry is one hook the install adds.
type entry struct {
	event, matcher string
	// timeout in seconds, 0 for the agent's default.
	timeout int
	// async hooks run in the background; they only send notices.
	async bool
	// was lists matchers earlier versions wrote for the event. A group of
	// ours with one of them gets the current matcher on install.
	was []string
}

// entries are the hooks for agent. The prompt hooks may wait up to maxWait
// for the phone, so the agent gets a minute more before it gives up on them.
func entries(agent Agent, maxWait time.Duration) []entry {
	wait := int((maxWait + time.Minute) / time.Second)
	if agent == Codex {
		return []entry{
			{event: "PermissionRequest", matcher: "*", timeout: wait},
			{event: "Stop", timeout: 60, async: true},
		}
	}
	return []entry{
		{event: "PermissionRequest", matcher: "*", timeout: wait},
		{event: "PreToolUse", matcher: "AskUserQuestion", timeout: wait},
		// Not idle_prompt: it comes about a minute after Stop with the same
		// collapse id, and its generic text would replace the finished notice.
		{event: "Notification", matcher: "agent_needs_input", async: true, was: []string{"idle_prompt|agent_needs_input"}},
		{event: "Stop", async: true},
	}
}

func (e entry) handler(command string) json.RawMessage {
	h := newObject()
	h.set("type", json.RawMessage(`"command"`))
	c, _ := marshal(command)
	h.set("command", c)
	if e.timeout > 0 {
		h.set("timeout", json.RawMessage(strconv.Itoa(e.timeout)))
	}
	if e.async {
		h.set("async", json.RawMessage("true"))
	}
	raw, _ := h.MarshalJSON()
	return raw
}

func (e entry) group(command string) json.RawMessage {
	g := newObject()
	if e.matcher != "" {
		m, _ := marshal(e.matcher)
		g.set("matcher", m)
	}
	g.set("hooks", json.RawMessage("["+string(e.handler(command))+"]"))
	raw, _ := g.MarshalJSON()
	return raw
}

// ours reports whether a hook handler runs this integration for agent.
func ours(handler json.RawMessage, agent Agent) bool {
	return strings.Contains(commandOf(handler), " agent hook "+string(agent))
}

func commandOf(handler json.RawMessage) string {
	var h struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(handler, &h)
	return h.Command
}

// hookLine matches a command line HookCommand writes: one path, the hook,
// and --max-wait when it is not the default.
var hookLine = regexp.MustCompile(`^('(?:[^']|'\\'')*'|[A-Za-z0-9_./@%+=:,-]+) agent hook (claude|codex)( --max-wait [0-9.hms]+)?$`)

// wrapped reports whether a handler of ours has a command install did not
// write, such as ours run through op run or env.
func wrapped(handler json.RawMessage) bool {
	return !hookLine.MatchString(commandOf(handler))
}

// Plan is a change to a settings file.
type Plan struct {
	// Path is the file written, with symlinks resolved, so a settings file
	// linked from a dotfiles repository stays a link.
	Path string
	// Before is nil when the file does not exist.
	Before []byte
	After  []byte
}

// Changed reports whether the plan changes anything but whitespace.
func (p Plan) Changed() bool {
	var a, b bytes.Buffer
	if json.Compact(&a, p.Before) != nil || json.Compact(&b, p.After) != nil {
		return !bytes.Equal(p.Before, p.After)
	}
	return !bytes.Equal(a.Bytes(), b.Bytes())
}

// Apply writes the plan. Before it changes an existing file it copies the
// file as it was to <file>.bak, unless a .bak is there already: that one is
// kept, so it stays the file from before the first install rather than the
// last. It returns the backup's path, "" for a file that did not exist, and
// whether this call wrote it. A plan that changes nothing writes nothing.
func (p Plan) Apply() (backup string, wrote bool, err error) {
	if !p.Changed() {
		return "", false, nil
	}
	mode := fs.FileMode(0o600)
	if p.Before != nil {
		if info, err := os.Stat(p.Path); err == nil {
			mode = info.Mode().Perm()
		}
		backup = p.Path + ".bak"
		switch _, err := os.Lstat(backup); {
		case errors.Is(err, fs.ErrNotExist):
			if err := writeFile(backup, p.Before, 0o600); err != nil {
				return "", false, err
			}
			wrote = true
		case err != nil:
			return "", false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return "", false, err
	}
	return backup, wrote, writeFile(p.Path, p.After, mode)
}

// writeFile replaces path atomically.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// settings is a settings file read for a change.
type settings struct {
	path   string
	before []byte
	root   *object
	hooks  *object
	// hadHooks: the file's hooks object held events before the change.
	hadHooks bool
	// indent is one level of the file's indent; "" for a file on one line.
	indent string
}

func readSettings(path string) (*settings, error) {
	s := &settings{path: path, indent: "  "}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		s.path = resolved
	}
	data, err := os.ReadFile(s.path) // #nosec G304 -- the user's own agent settings file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.root, s.hooks = newObject(), newObject()
		return s, nil
	case err != nil:
		return nil, err
	}
	s.before, s.indent = data, indentOf(data)
	if s.root, err = parseObject(data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	s.hooks = newObject()
	if raw, ok := s.root.get("hooks"); ok {
		if s.hooks, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s: hooks: %w", s.path, err)
		}
		s.hadHooks = len(s.hooks.keys) > 0
	}
	return s, nil
}

// indentOf is the indent of the first indented line of a JSON object,
// which is one level deep: two spaces when none is or the object is empty,
// "" for one written on one line.
func indentOf(data []byte) string {
	lines := bytes.Split(bytes.TrimRight(data, "\r\n"), []byte("\n"))
	if len(lines) == 1 {
		var c bytes.Buffer
		if json.Compact(&c, data) == nil && c.String() == "{}" {
			return "  "
		}
		return ""
	}
	for _, line := range lines[1:] {
		rest := bytes.TrimLeft(line, " \t")
		if n := len(line) - len(rest); n > 0 && len(bytes.TrimSpace(rest)) > 0 {
			return string(line[:n])
		}
	}
	return "  "
}

// plan writes the hooks back into the root object and formats the file the
// way it was: its indent (two spaces for a new file, the way the agents
// write theirs), keys in their order, a final newline if it had one.
func (s *settings) plan() (Plan, error) {
	switch {
	case len(s.hooks.keys) > 0:
		raw, err := s.hooks.MarshalJSON()
		if err != nil {
			return Plan{}, err
		}
		s.root.set("hooks", raw)
	case s.hadHooks:
		// Uninstall took out the last of them.
		s.root.del("hooks")
	}
	raw, err := s.root.MarshalJSON()
	if err != nil {
		return Plan{}, err
	}
	var out bytes.Buffer
	if s.indent == "" {
		err = json.Compact(&out, raw)
	} else {
		err = json.Indent(&out, raw, "", s.indent)
	}
	if err != nil {
		return Plan{}, err
	}
	if s.before == nil || bytes.HasSuffix(s.before, []byte("\n")) {
		out.WriteByte('\n')
	}
	p := Plan{Path: s.path, Before: s.before, After: out.Bytes()}
	if !p.Changed() {
		p.After = p.Before
	}
	return p, nil
}

// PlanInstall adds agent's hooks to the settings file at path, running
// command. An entry of ours already there is updated in place, so running it
// again changes nothing.
func PlanInstall(path string, agent Agent, command string, maxWait time.Duration) (Plan, error) {
	s, err := readSettings(path)
	if err != nil {
		return Plan{}, err
	}
	for _, e := range entries(agent, maxWait) {
		var groups []json.RawMessage
		if raw, ok := s.hooks.get(e.event); ok {
			if groups, err = array(raw); err != nil {
				return Plan{}, fmt.Errorf("%s: hooks.%s: %w", s.path, e.event, err)
			}
		}
		want := e.handler(command)
		found := false
		groups, _, err = editHandlers(groups, func(h json.RawMessage) (json.RawMessage, bool) {
			switch {
			case !ours(h, agent):
				return h, true
			case found:
				return nil, false
			}
			found = true
			if wrapped(h) {
				return h, true
			}
			return want, true
		})
		if err != nil {
			return Plan{}, err
		}
		if !found {
			groups = append(groups, e.group(command))
		}
		if groups, err = rematch(groups, agent, e); err != nil {
			return Plan{}, err
		}
		raw, err := marshal(groups)
		if err != nil {
			return Plan{}, err
		}
		s.hooks.set(e.event, raw)
	}
	return s.plan()
}

// rematch gives the groups of ours that still carry a matcher an earlier
// version wrote the entry's current one. A group that also holds someone
// else's handler keeps its matcher, which that handler may rely on.
func rematch(groups []json.RawMessage, agent Agent, e entry) ([]json.RawMessage, error) {
	if len(e.was) == 0 {
		return groups, nil
	}
	for i, g := range groups {
		obj, err := parseObject(g)
		if err != nil {
			continue
		}
		var matcher string
		if raw, ok := obj.get("matcher"); !ok || json.Unmarshal(raw, &matcher) != nil || !slices.Contains(e.was, matcher) {
			continue
		}
		raw, _ := obj.get("hooks")
		handlers, err := array(raw)
		if err != nil || len(handlers) == 0 || slices.ContainsFunc(handlers, func(h json.RawMessage) bool { return !ours(h, agent) }) {
			continue
		}
		m, err := marshal(e.matcher)
		if err != nil {
			return nil, err
		}
		obj.set("matcher", m)
		if groups[i], err = obj.MarshalJSON(); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

// PlanUninstall takes agent's hooks out of the settings file at path, and
// the groups and events that held nothing else.
func PlanUninstall(path string, agent Agent) (Plan, error) {
	s, err := readSettings(path)
	if err != nil || s.before == nil {
		return Plan{Path: path}, err
	}
	for _, event := range append([]string(nil), s.hooks.keys...) {
		raw, _ := s.hooks.get(event)
		groups, err := array(raw)
		if err != nil {
			continue
		}
		kept, changed, err := editHandlers(groups, func(h json.RawMessage) (json.RawMessage, bool) {
			return h, !ours(h, agent)
		})
		switch {
		case err != nil:
			return Plan{}, err
		case !changed:
		case len(kept) == 0:
			s.hooks.del(event)
		default:
			raw, err := marshal(kept)
			if err != nil {
				return Plan{}, err
			}
			s.hooks.set(event, raw)
		}
	}
	return s.plan()
}

// Installed lists the events of the settings file at path that run agent's
// hook.
func Installed(path string, agent Agent) ([]string, error) {
	s, err := readSettings(path)
	if err != nil {
		return nil, err
	}
	var events []string
	for _, event := range s.hooks.keys {
		raw, _ := s.hooks.get(event)
		groups, err := array(raw)
		if err != nil {
			continue
		}
		found := false
		if _, _, err := editHandlers(groups, func(h json.RawMessage) (json.RawMessage, bool) {
			found = found || ours(h, agent)
			return h, true
		}); err == nil && found {
			events = append(events, event)
		}
	}
	return events, nil
}

// editHandlers runs fn over every hook handler in an event's groups. fn
// returns the handler to keep in its place, or false to drop it. A group
// left with no handlers is dropped, and groups fn did not change keep their
// bytes. It reports whether anything changed.
func editHandlers(groups []json.RawMessage, fn func(json.RawMessage) (json.RawMessage, bool)) ([]json.RawMessage, bool, error) {
	out := make([]json.RawMessage, 0, len(groups))
	changed := false
	for _, g := range groups {
		obj, err := parseObject(g)
		if err != nil {
			out = append(out, g)
			continue
		}
		raw, ok := obj.get("hooks")
		if !ok {
			out = append(out, g)
			continue
		}
		handlers, err := array(raw)
		if err != nil {
			out = append(out, g)
			continue
		}
		kept := make([]json.RawMessage, 0, len(handlers))
		touched := false
		for _, h := range handlers {
			nh, keep := fn(h)
			if !keep {
				touched = true
				continue
			}
			touched = touched || !bytes.Equal(nh, h)
			kept = append(kept, nh)
		}
		if !touched {
			out = append(out, g)
			continue
		}
		changed = true
		if len(kept) == 0 {
			continue
		}
		hraw, err := marshal(kept)
		if err != nil {
			return nil, false, err
		}
		obj.set("hooks", hraw)
		graw, err := obj.MarshalJSON()
		if err != nil {
			return nil, false, err
		}
		out = append(out, graw)
	}
	return out, changed, nil
}
