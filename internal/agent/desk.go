package agent

// Whether the user is at the computer the agent runs on. At the desk the
// prompt stays on the terminal; away, it goes to the phone, and coming back
// while it waits there hands it back to the terminal.

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"
)

// Idler reads how long ago this computer last had a key press, click or
// touch.
type Idler func(ctx context.Context) (time.Duration, error)

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
