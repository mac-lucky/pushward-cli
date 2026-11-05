package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
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

// cutMiddle shortens s to at most n runes by taking out its middle, about a
// third of the room going to the end, so the tail of a command (a `| sh`,
// say) still shows next to Allow.
func cutMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	tail := (n - 3) / 3
	return string(r[:n-3-tail]) + "..." + string(r[len(r)-tail:])
}

const masked = "***"

var (
	// Tokens known by their prefix, whole. A console session (hls_) is
	// base62 only, which ffmpeg's -hls_segment_filename and the like are not.
	prefixedToken = regexp.MustCompile(`\b(?:(?:hlk_|hla_|hlw_|ghp_|gho_|ghs_|github_pat_|sk-|xoxb-|xoxp-|AKIA)[A-Za-z0-9_-]{8,}|hls_[A-Za-z0-9]{20,})`)
	// Bearer or Basic and its credentials.
	authScheme = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)([A-Za-z0-9._~+/=-]{8,})`)
	// An Authorization header with any scheme (Token, Bot, Digest...): the
	// scheme stays, what follows it goes.
	authHeader = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)((?:[A-Za-z][A-Za-z0-9_-]*\s+)?)([^\s"']+)`)
	// The password of scheme://user:pass@host.
	urlPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/@]*:)[^\s@/]+@`)
	// NAME=value, name: value, "name": "value", --name=value.
	namedValue = regexp.MustCompile(`(?i)([A-Za-z0-9_-]*(?:token|secret|password|passwd|api_key|api-key|apikey|auth)[A-Za-z0-9_-]*)(["']?\s*[:=]\s*)("(?:[^"\\]|\\.)*"|"[^"]*"|'[^']*'|[^\s"',;&|)}\]]+)`)
	// --name value; the dash starts a word, so multi-token is not a flag.
	flagValue = regexp.MustCompile(`(?i)\B(--?[A-Za-z0-9_-]*(?:token|secret|password|passwd|api_key|api-key|apikey|auth)[A-Za-z0-9_-]*)(\s+)("(?:[^"\\]|\\.)*"|"[^"]*"|'[^']*'|[^\s"'<>|&;-][^\s"'<>|&;]*)`)
	// aws configure set aws_secret_access_key <value>: the name is a bare
	// word, and the value has a key's alphabet and length, not a path's.
	awsPair = regexp.MustCompile(`(?i)\b(aws_secret_access_key|aws_session_token)(\s+)("[A-Za-z0-9/+=]{16,}"|'[A-Za-z0-9/+=]{16,}'|[A-Za-z0-9/+=]{16,})`)
	// What follows the colon of curl's -u or --user, up to the end of
	// that command; docker run -u with a uid and gid is not curl's.
	userPassword = regexp.MustCompile(`\b((?:curl|wget)\b[^|;&\n]*?\s(?:-u\s*|--user(?:\s+|=))["']?[^\s:"']*:)([^\s"'$;&|<>()\x60][^\s"';&|<>()\x60]*)`)
	// The body of a PEM private key: base64 lines (or a literal \n between
	// them) to its END line or, cut short, the base64 runs that follow.
	// Anything else in between, a command say, stops it and stays readable.
	pemKey = regexp.MustCompile(`(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----)(?:(?:[A-Za-z0-9+/=\s]|\\n)*?(-----END [A-Z0-9 ]*PRIVATE KEY-----)|(?:\s|\\n)*[A-Za-z0-9+/=]{16,}(?:(?:\s|\\n)+[A-Za-z0-9+/=]{16,})*)`)
	// author, authors, authorship and authority are names, not credentials.
	authorWord = regexp.MustCompile(`(?i)author(?:s|ship|ity|ities)?(?:[^a-z]|$)`)
	secretWord = regexp.MustCompile(`(?i)token|secret|password|passwd|api_key|api-key|apikey|auth`)
)

// maskSecrets replaces the values that look like credentials in text bound
// for the card or a notification with ***. It is a net for the obvious ones,
// not a guarantee.
func maskSecrets(s string) string {
	s = pemKey.ReplaceAllString(s, "${1}\n"+masked+"\n${2}")
	s = prefixedToken.ReplaceAllString(s, masked)
	s = authScheme.ReplaceAllStringFunc(s, func(m string) string {
		p := authScheme.FindStringSubmatch(m)
		if !credentialLike(p[3]) {
			return m
		}
		return p[1] + p[2] + masked
	})
	s = urlPassword.ReplaceAllString(s, "${1}"+masked+"@")
	s = userPassword.ReplaceAllString(s, "${1}"+masked)
	s = authHeader.ReplaceAllStringFunc(s, func(m string) string {
		p := authHeader.FindStringSubmatch(m)
		if strings.HasPrefix(p[3], masked) {
			return m
		}
		return p[1] + p[2] + masked
	})
	for _, re := range []*regexp.Regexp{namedValue, flagValue, awsPair} {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			p := re.FindStringSubmatch(m)
			name, sep, value := p[1], p[2], p[3]
			quote := ""
			if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
				quote, value = value[:1], value[1:len(value)-1]
			}
			switch {
			case !secretWord.MatchString(authorWord.ReplaceAllString(name, "")),
				value == "", strings.HasPrefix(value, masked),
				// An Authorization header, masked above.
				re == namedValue && strings.HasSuffix(strings.ToLower(name), "authorization"),
				// A scheme whose credentials were masked above.
				strings.EqualFold(value, "bearer"), strings.EqualFold(value, "basic"):
				return m
			}
			return name + sep + quote + masked + quote
		})
	}
	return s
}

// credentialLike tells credentials after Bearer or Basic from words: they
// have a digit or a symbol, or a capital past the first letter.
func credentialLike(s string) bool {
	return strings.ContainsAny(s, "0123456789+/=._~-") || strings.ToLower(s[1:]) != s[1:]
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
	// A command's text keeps its end; a path's or a name's its start.
	textCut := cut
	switch tool {
	case "Bash", "PowerShell":
		if value = shellCommand(args["command"]); value != "" {
			state, text = "Run a command", "Run: "+value
			textCut = cutMiddle
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
	s := summary{state: cut(oneLine(maskSecrets(state)), 256), text: textCut(oneLine(maskSecrets(text)), 256)}
	if value = oneLine(maskSecrets(value)); value != "" {
		s.row = &detail{Label: cut(oneLine(tool), 24), Value: cutMiddle(value, 64)}
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

// scriptFlags precede the script in a shell's argv.
var scriptFlags = []string{"-c", "-lc", "/c", "/C", "-Command", "-command"}

// shells run their last argument as the script. Any other program
// (perl -e... -c, ./payload -c) shows whole.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh", "fish", "pwsh", "powershell", "cmd"}

// shellCommand reads a command that is a string, or an argv array (Codex
// sends ["bash", "-lc", "<script>"], and on Windows
// ["powershell.exe", "-NoProfile", "-Command", "<script>"]; the script is
// what the user reads). Only flags may come between the shell and its
// script: a wrapper such as sudo, ssh host or kubectl exec shows whole.
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
		if n := len(args); n >= 3 && isShell(args[0]) && slices.Contains(scriptFlags, args[n-2]) && allFlags(args[0], args[1:n-2]) {
			return args[n-1]
		}
		return strings.Join(args, " ")
	}
	return ""
}

// isShell reports whether a program path names one of shells.
func isShell(prog string) bool {
	return slices.Contains(shells, progName(prog))
}

// progName is a program path's last element, split on either separator,
// lower case and without .exe.
func progName(prog string) string {
	return strings.TrimSuffix(strings.ToLower(prog[strings.LastIndexAny(prog, `/\`)+1:]), ".exe")
}

// valueFlags take the next argument as their value, which may run before
// the script: an rc file, a shell option, a script file, an encoded command.
// PowerShell's are matched in any case, and -o matches bash's -O too.
var valueFlags = []string{"--rcfile", "--init-file", "-o", "-file", "-encodedcommand"}

// allFlags reports whether every arg of shell is a flag that takes no value:
// -x, or for cmd a switch such as /d, which unlike a path (/bin/sh) has no
// second slash. A flag carrying a value (--rcfile=x, --rcfile x) does not
// count: the value may run before the script.
func allFlags(shell string, args []string) bool {
	name := progName(shell)
	cmd, pwsh := name == "cmd", name == "pwsh" || name == "powershell"
	for _, a := range args {
		cmdSwitch := cmd && strings.HasPrefix(a, "/") && !strings.Contains(a[1:], "/")
		valueFlag := slices.ContainsFunc(valueFlags, func(f string) bool { return strings.EqualFold(a, f) }) ||
			pwsh && psValueFlag(a)
		if !strings.HasPrefix(a, "-") && !cmdSwitch || strings.Contains(a, "=") || valueFlag {
			return false
		}
	}
	return true
}

// psValueFlag reports whether a PowerShell flag is -File or -EncodedCommand
// by any prefix PowerShell accepts for them (-f, -e, -enc...) or -ec.
func psValueFlag(a string) bool {
	n := strings.ToLower(strings.TrimLeft(a, "-"))
	return n != "" && (strings.HasPrefix("file", n) || strings.HasPrefix("encodedcommand", n) || n == "ec")
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
