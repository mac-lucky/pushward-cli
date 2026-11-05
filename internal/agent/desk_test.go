package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// A proc tree as Linux shows it: the hook's shell with pipes, Claude Code on
// a pty under tmux, and init.
func TestTerminalOf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	proc := t.TempDir()
	mk := func(pid, ppid int, comm string, fds ...string) {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194560", pid, comm, ppid, pid, pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		for fd, target := range fds {
			if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A name with ") S 1 (" in it: read from the front, the parent would be 1.
	mk(300, 200, "sh) S 1 (x", "pipe:[81]", "pipe:[82]", "/dev/tty")
	mk(200, 100, "claude", "/dev/null", "/dev/pts/4", "/dev/pts/4")
	mk(100, 1, "tmux: client", "/dev/pts/9", "/dev/pts/9", "/dev/pts/9")
	mk(1, 0, "systemd", "/dev/null", "/dev/null", "/dev/null")
	mk(400, 1, "cron", "/dev/null", "pipe:[90]", "/dev/pts/ptmx")
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ssh := env(map[string]string{"SSH_TTY": "/dev/pts/2"})
	for _, c := range []struct {
		pid    int
		getenv func(string) string
		want   string
	}{
		// The proc tree wins over $SSH_TTY, which a reattached tmux keeps
		// from an older login.
		{300, ssh, "/dev/pts/4"},
		{200, env(nil), "/dev/pts/4"},
		{400, ssh, "/dev/pts/2"},
		{400, env(nil), ""},
		{999, env(nil), ""},
	} {
		got, err := terminalOf(proc, c.pid, c.getenv)
		if c.want == "" {
			if !errors.Is(err, ErrNoTerminal) {
				t.Errorf("pid %d: %q, %v; want ErrNoTerminal", c.pid, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("pid %d: %q, %v; want %s", c.pid, got, err, c.want)
		}
	}
}

func TestParentPID(t *testing.T) {
	for stat, want := range map[string]int{
		"42 (bash) S 7 42 42 34816 42 4194304": 7,
		"42 (a) b (c)) R 9 1 1":                9,
		"42 (bash)":                            0,
		"garbage":                              0,
	} {
		if got := parentPID([]byte(stat)); got != want {
			t.Errorf("parentPID(%q) = %d, want %d", stat, got, want)
		}
	}
}

// A process table as ps shows it on a Mac: the hook's shell with no
// terminal, Claude Code on a tmux pane, the tmux server with none, launchd.
func TestControllingTTY(t *testing.T) {
	table := map[int]struct {
		ppid int
		tty  string
	}{
		300: {200, "??"},
		200: {150, "ttys004"},
		150: {1, "??"},
		90:  {1, "??"},
		// A table that loops.
		70: {71, "??"},
		71: {70, "??"},
	}
	proc := func(_ context.Context, pid int) (int, string, error) {
		p, ok := table[pid]
		if !ok {
			return 0, "", errors.New("no such process")
		}
		return p.ppid, p.tty, nil
	}
	for pid, want := range map[int]string{300: "/dev/ttys004", 200: "/dev/ttys004", 90: "", 70: "", 404: "", 0: ""} {
		if got := controllingTTY(context.Background(), pid, proc); got != want {
			t.Errorf("pid %d: %q, want %q", pid, got, want)
		}
	}
}

func TestParsePS(t *testing.T) {
	for out, want := range map[string]string{
		" 2815 ttys011 \n": "2815 ttys011",
		"38410 ??\n":       "38410 ??",
		"":                 "error",
		"x ttys001":        "error",
	} {
		ppid, tty, err := parsePS([]byte(out))
		got := fmt.Sprintf("%d %s", ppid, tty)
		if err != nil {
			got = "error"
		}
		if got != want {
			t.Errorf("parsePS(%q) = %s, want %s", out, got, want)
		}
	}
}

// GetLastInputInfo's tick count wraps every 49.7 days; the difference does
// not.
func TestSinceTick(t *testing.T) {
	for _, c := range []struct {
		now, then uint32
		want      time.Duration
	}{
		{90_000, 30_000, time.Minute},
		{5, math.MaxUint32 - 4, 10 * time.Millisecond},
		{7, 7, 0},
	} {
		if got := sinceTick(c.now, c.then); got != c.want {
			t.Errorf("sinceTick(%d, %d) = %s, want %s", c.now, c.then, got, c.want)
		}
	}
	// GetTickCount64 truncated to 32 bits, across the wrap.
	now64 := uint64(1)<<32 + 2_000
	if got := sinceTick(uint32(now64), uint32(now64-5_000)); got != 5*time.Second {
		t.Errorf("across the wrap: %s", got)
	}
}
