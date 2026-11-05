// Package agent answers Claude Code and Codex hooks from PushWard. A
// permission prompt, or one of Claude's questions, goes to the user's phone
// as an approval Live Activity (an actionable notification when no device
// shows the card), and the answer comes back as the hook's decision. Notices
// tell the user that a session needs them or has finished.
//
// Nothing here logs a command, a file's contents, typed text, an answer URL
// or a key: log lines carry slugs, ids and statuses only.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

// Agent is the coding agent whose hook is running.
type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

// Name is how the agent is called on the card and in notifications.
func (a Agent) Name() string {
	if a == Codex {
		return "Codex"
	}
	return "Claude Code"
}

// Caller sends one operation of the PushWard API and returns the response
// body. The cmd package backs it with the CLI's own client.
type Caller func(ctx context.Context, op string, params map[string]string, query url.Values, body map[string]any) ([]byte, error)

// ErrNoKey is what a Caller returns when it has no usable API key.
var ErrNoKey = errors.New("no usable API key")

// Options are the hook's flags.
type Options struct {
	// MaxWait is how long a prompt waits for an answer from the phone.
	MaxWait time.Duration
	// OnTimeout is none (hand the prompt back to the terminal) or deny.
	OnTimeout string
	// DeskIdle is how long the computer, or on Linux the agent's terminal,
	// must have had no input before the user counts as away from it.
	DeskIdle time.Duration
	// Desk is auto (read the idle time), away or desk.
	Desk string
	// Via is auto (a card, a notification when no device shows it), card
	// or notification.
	Via string
	// Notices is away (only when away from the desk), always or off.
	Notices string
}

const (
	DefaultMaxWait  = 25 * time.Minute
	DefaultDeskIdle = 90 * time.Second
)

func DefaultOptions() Options {
	return Options{MaxWait: DefaultMaxWait, OnTimeout: "none", DeskIdle: DefaultDeskIdle, Desk: "auto", Via: "auto", Notices: "away"}
}

// Validate checks the enumerated options and the durations.
func (o Options) Validate() error {
	for _, c := range []struct {
		flag, value string
		allowed     []string
	}{
		{"on-timeout", o.OnTimeout, []string{"none", "deny"}},
		{"desk", o.Desk, []string{"auto", "away", "desk"}},
		{"via", o.Via, []string{"auto", "card", "notification"}},
		{"notices", o.Notices, []string{"away", "always", "off"}},
	} {
		ok := false
		for _, a := range c.allowed {
			ok = ok || c.value == a
		}
		if !ok {
			return fmt.Errorf("--%s: want one of %v, got %q", c.flag, c.allowed, c.value)
		}
	}
	if o.MaxWait < time.Second {
		return errors.New("--max-wait must be at least 1s")
	}
	if o.DeskIdle < 0 {
		return errors.New("--desk-idle must not be negative")
	}
	return nil
}

// Runner answers hooks.
type Runner struct {
	Call Caller
	// Idle reads the time since the last input at this computer; nil where
	// there is no way to read it, which counts as away.
	Idle Idler
	// Log receives ids and statuses; nil logs nothing.
	Log  *log.Logger
	Opts Options
	// KeyCache, when set, lets a key that passed the check in the last hour
	// skip it.
	KeyCache *KeyCache
	// Dialogs, when set, is the directory where hooks leave a file for each
	// dialog they take to the phone or send a notice for, so the
	// permission_prompt Claude Code sends for that dialog is not pushed again.
	Dialogs string
	// Getenv reads the agent's environment, which the hook inherits; nil
	// reads the process's own.
	Getenv func(string) string

	keyOnce sync.Once
	keyErr  error
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// describe is an error fit for the log. API errors keep their method, path
// (slugs and ids), status and code but not their detail, which can quote the
// card's text back; anything else is reduced to its kind.
func describe(err error) string {
	var ae *api.Error
	var te *api.TransportError
	switch {
	case errors.As(err, &ae):
		s := fmt.Sprintf("%s %s: %d", ae.Method, ae.Path, ae.Status)
		if ae.Code != "" {
			s += " " + ae.Code
		}
		return s
	case errors.As(err, &te):
		return fmt.Sprintf("%s %s: network error", te.Method, te.Path)
	case errors.Is(err, ErrNoKey):
		return ErrNoKey.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err.Error()
	case err == nil:
		return "ok"
	}
	return "internal error"
}

// Hook answers one hook call. stdin is the JSON object the agent wrote; the
// result is what to print on stdout, nil for nothing. It cannot fail: on any
// error it logs and returns what leaves the prompt to the terminal.
func (r *Runner) Hook(ctx context.Context, agent Agent, stdin io.Reader) []byte {
	in, err := readInput(stdin)
	if err != nil {
		r.logf("unreadable hook input")
		return nil
	}
	switch in.Event {
	case "PermissionRequest":
		return r.permission(ctx, agent, in)
	case "PreToolUse":
		if agent == Claude && in.ToolName == "AskUserQuestion" {
			return r.questions(ctx, in)
		}
	case "Notification":
		if agent == Claude {
			r.needsInput(ctx, in)
		}
	case "Stop":
		r.finished(ctx, agent, in)
		if agent == Codex {
			// Codex wants JSON from a Stop hook that exits 0.
			return []byte("{}\n")
		}
	}
	return nil
}

// encode writes a hook decision as one line of JSON. Quotes and angle
// brackets in echoed text stay as they are.
func encode(v any) []byte {
	b, err := marshal(v)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
