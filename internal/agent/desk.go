package agent

// Whether the user is at the computer the agent runs on. At the desk the
// prompt stays on the terminal; away, it goes to the phone, and coming back
// while it waits there hands it back to the terminal.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Idler reads how long ago this computer last had a key press, click or
// touch, or on Linux and over SSH, how long ago the agent's terminal last
// read a key.
type Idler func(ctx context.Context) (time.Duration, error)

// ErrNoTerminal is what an Idler returns when the agent runs with no
// terminal, as a `claude -p` under cron does on Linux: the user counts as
// away.
var ErrNoTerminal = errors.New("no terminal to read")

const (
	// deskEvery is how often a wait on the phone reads the idle time.
	deskEvery = 2 * time.Second
	// backAt: idle less than this during a wait means the user is back.
	backAt = 5 * time.Second
	// A read slower than idleTimeout counts as failed.
	idleTimeout = 2 * time.Second
)

func (r *Runner) idle(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, idleTimeout)
	defer cancel()
	return r.Idle(ctx)
}

// atDesk reports whether the user is at the computer now. A read that fails
// counts as away: a broken probe costs a push at the desk, never a prompt
// missed away from it.
func (r *Runner) atDesk(ctx context.Context) bool {
	switch {
	case r.Opts.Desk == "desk":
		r.logf("at the desk (--desk desk), leaving it to the terminal")
		return true
	case r.Opts.Desk == "away" || r.Idle == nil:
		return false
	}
	idle, err := r.idle(ctx)
	if err != nil {
		r.logf("could not read the idle time, counting as away")
		return false
	}
	if idle < r.Opts.DeskIdle {
		r.logf("at the desk (idle %s), leaving it to the terminal", idle.Round(time.Second))
		return true
	}
	return false
}

// watchesDesk reports whether waits on the phone watch for the user's
// return.
func (r *Runner) watchesDesk() bool {
	return r.Opts.Desk == "auto" && r.Idle != nil
}

// backAtDesk reports whether there was input at the computer moments ago.
func (r *Runner) backAtDesk(ctx context.Context) bool {
	idle, err := r.idle(ctx)
	return err == nil && idle < backAt
}

var hidIdleTime = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// parseHIDIdle reads HIDIdleTime, in nanoseconds, out of what
// `ioreg -c IOHIDSystem` prints.
func parseHIDIdle(out []byte) (time.Duration, error) {
	m := hidIdleTime.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("no HIDIdleTime in what ioreg printed")
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

// terminalOf finds the terminal of the agent that ran the hook: the first of
// fds 0, 1 and 2 that is a terminal, on pid and then on each parent up the
// tree, read from proc (/proc on Linux). $SSH_TTY is only the fallback: a
// tmux session reattached from a later login keeps the first login's.
func terminalOf(proc string, pid int, getenv func(string) string) (string, error) {
	// Bounded, in case a broken proc tree loops.
	for range 64 {
		if pid <= 0 {
			break
		}
		dir := filepath.Join(proc, strconv.Itoa(pid))
		for fd := range 3 {
			if link, err := os.Readlink(filepath.Join(dir, "fd", strconv.Itoa(fd))); err == nil && isTerminal(link) {
				return link, nil
			}
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat")) // #nosec G304 G703 -- a file under /proc
		if err != nil {
			break
		}
		pid = parentPID(stat)
	}
	if tty := getenv("SSH_TTY"); tty != "" {
		return tty, nil
	}
	return "", ErrNoTerminal
}

// controllingTTY is the controlling terminal of pid or, when it has none, of
// the first process above it that has one; "" when none does. proc reads one
// process: its parent's pid and its terminal as ps names it (ttys003, or ??
// for none).
func controllingTTY(ctx context.Context, pid int, proc func(context.Context, int) (int, string, error)) string {
	// Bounded, in case a broken process table loops.
	for range 16 {
		if pid <= 1 {
			break
		}
		ppid, tty, err := proc(ctx, pid)
		if err != nil {
			break
		}
		if strings.HasPrefix(tty, "tty") {
			return "/dev/" + tty
		}
		pid = ppid
	}
	return ""
}

// parsePS reads what `ps -o ppid=,tty= -p <pid>` prints.
func parsePS(out []byte) (int, string, error) {
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, "", errors.New("no ppid and tty in what ps printed")
	}
	ppid, err := strconv.Atoi(f[0])
	return ppid, f[1], err
}

// isTerminal reports whether a device path is a terminal: /dev/pts/N or
// /dev/ttyX, but not /dev/tty (whoever opens it gets their own) or ptmx.
func isTerminal(path string) bool {
	rest, ok := strings.CutPrefix(path, "/dev/pts/")
	if !ok {
		rest, ok = strings.CutPrefix(path, "/dev/tty")
	}
	return ok && rest != "" && rest != "ptmx"
}

// parentPID reads the parent's pid out of /proc/<pid>/stat, 0 when it
// cannot. The command name in parentheses may hold spaces and parentheses
// itself, so the fields count from the last ')'.
func parentPID(stat []byte) int {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 2 {
		return 0
	}
	pid, _ := strconv.Atoi(f[1])
	return pid
}

// sinceTick is the time between two of Windows' millisecond tick counts,
// then and now, as GetLastInputInfo keeps them: truncated to 32 bits, so the
// unsigned difference stays right when the count wraps every 49.7 days.
func sinceTick(now, then uint32) time.Duration {
	return time.Duration(now-then) * time.Millisecond
}
