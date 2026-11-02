package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxInput bounds what a hook reads from stdin. A Write tool call carries
// the whole file it writes.
const maxInput = 32 << 20

// Input is the JSON object Claude Code and Codex write on a hook's stdin.
// Fields an event does not have stay empty.
type Input struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`

	// PermissionRequest and PreToolUse.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// Claude's PermissionRequest: the permission updates it would apply for
	// "always allow".
	Suggestions []json.RawMessage `json:"permission_suggestions"`

	// Claude's Notification.
	Message          string `json:"message"`
	NotificationType string `json:"notification_type"`

	// Stop, on both agents; null on Codex when there is none.
	LastAssistantMessage *string `json:"last_assistant_message"`
}

func readInput(r io.Reader) (Input, error) {
	var in Input
	data, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return in, err
	}
	if len(data) > maxInput {
		return in, errors.New("hook input too large")
	}
	err = json.Unmarshal(data, &in)
	return in, err
}

// repo is the name the card gives the session: the working directory's
// last element.
func (in Input) repo() string {
	if in.Cwd == "" {
		return ""
	}
	return cut(filepath.Base(filepath.Clean(in.Cwd)), 24)
}

// hex8 is the first 8 hex digits of s's SHA-256.
func hex8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// slug names the card of one prompt: the session, then the tool call. The
// same call in the same session gets the same card.
func (in Input) slug() string {
	return "agent-" + hex8(in.SessionID) + "-" + hex8(in.ToolName+"\n"+canonical(in.ToolInput))
}

// noticeID is the collapse id of a session's notices, so a newer one
// replaces the older on the devices.
func (in Input) noticeID() string {
	return "agent-" + hex8(in.SessionID)
}

// canonical is raw JSON with the object keys sorted and no whitespace, so the
// same tool input always digests the same.
func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// cut shortens s to at most n runes, ending in "..." when it had to cut.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// oneLine collapses every run of whitespace, newlines included, to a space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// detail is one label/value row of an approval card.
type detail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// summary is what a permission prompt shows. The card says what the tool
// wants to do in a few words and names the command or file once, in the
// tool's row; the notification has no row, so its text carries the command.
type summary struct {
	state string
	text  string
	row   *detail
}

// summarize describes a tool call: "Run a command" for a shell command,
// "Edit <file name>" for a change to a file, the tool's name for anything
// else.
func (in Input) summarize() summary {
	var args map[string]any
	_ = json.Unmarshal(in.ToolInput, &args)
	tool := in.ToolName
	var state, text, value string
	switch tool {
	case "Bash":
		if value = shellCommand(args["command"]); value != "" {
			state, text = "Run a command", "Run: "+value
		}
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		if path := firstString(args, "file_path", "notebook_path"); path != "" {
			value = in.relative(path)
			state, text = "Edit "+filepath.Base(path), "Edit "+value
		}
	case "apply_patch":
		if files := patchFiles(shellCommand(args["command"])); len(files) > 0 {
			value = in.relative(files[0])
			state, text = "Edit "+filepath.Base(files[0]), "Edit "+value
			if len(files) > 1 {
				more := " and " + plural(len(files)-1, "more file")
				state, text = state+more, text+more
			}
		}
	default:
		value = firstString(args, "command", "file_path", "path", "url", "pattern", "query")
		if value == "" && len(args) > 0 {
			b, _ := json.Marshal(args)
			value = string(b)
		}
	}
	if state == "" {
		state = tool
	}
	if text == "" {
		text = state
	}
	s := summary{state: cut(oneLine(state), 256), text: cut(oneLine(text), 256)}
	if value = oneLine(value); value != "" {
		s.row = &detail{Label: cut(oneLine(tool), 24), Value: cut(value, 64)}
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// shellCommand reads a command that is a string, or an argv array (Codex
// sends ["bash", "-lc", "<script>"]; the script is what the user reads).
func shellCommand(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		args := make([]string, 0, len(c))
		for _, a := range c {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
		if len(args) == 3 && (args[1] == "-c" || args[1] == "-lc") {
			return args[2]
		}
		return strings.Join(args, " ")
	}
	return ""
}

// patchFiles lists the files an apply_patch envelope touches, in order.
func patchFiles(patch string) []string {
	var files []string
	for line := range strings.SplitSeq(patch, "\n") {
		for _, p := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if f, ok := strings.CutPrefix(strings.TrimSpace(line), p); ok && f != "" {
				files = append(files, f)
			}
		}
	}
	return files
}

// relative shows a path inside the working directory relative to it.
func (in Input) relative(path string) string {
	if in.Cwd == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(in.Cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// question is one entry of AskUserQuestion's tool input.
type question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []questionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

func (in Input) questions() ([]question, error) {
	var ti struct {
		Questions []question `json:"questions"`
	}
	err := json.Unmarshal(in.ToolInput, &ti)
	return ti.Questions, err
}
