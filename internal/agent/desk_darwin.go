//go:build darwin

package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// SystemIdle reads the Mac's idle time. macOS keeps the time since the last
// key, click or touch as IOHIDSystem's HIDIdleTime, and ioreg prints it,
// which spares an IOKit binding and cgo. Over SSH the Mac's own keyboard is
// not the user's, so the agent's terminal is read instead, as on Linux: the
// controlling terminal of the hook's parent, or of the first process above
// it that has one. That is the SSH login's terminal, or the tmux pane the
// agent runs in. SSH_TTY is not read: a tmux pane keeps the one of the login
// that started the server, which may be gone, its node reused by another.
func SystemIdle() Idler {
	var once sync.Once
	var tty string
	return func(ctx context.Context) (time.Duration, error) {
		// shortcut: a tmux session started over SSH and attached at the Mac
		// later keeps SSH_CONNECTION, so there only keys in the agent's
		// terminal count, not the mouse; also read HIDIdleTime if that bites.
		if os.Getenv("SSH_CONNECTION") != "" {
			once.Do(func() { tty = controllingTTY(ctx, os.Getppid(), psProc) })
			if tty != "" {
				if d, err := ttyIdle(tty, time.Now()); err == nil {
					return d, nil
				}
			}
		}
		out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem", "-r", "-k", "HIDIdleTime", "-d", "1").Output()
		if err != nil {
			return 0, err
		}
		return parseHIDIdle(out)
	}
}

// psProc reads one process's parent and terminal with ps.
func psProc(ctx context.Context, pid int) (int, string, error) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- a pid
	if err != nil {
		return 0, "", err
	}
	return parsePS(out)
}
