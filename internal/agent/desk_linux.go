package agent

import (
	"context"
	"os"
	"time"
)

// SystemIdle reads how long the agent's terminal has gone without a key
// press. A desktop session and an SSH login read the same way, and a key
// pressed in the terminal while a prompt waits on the phone hands it back.
func SystemIdle() Idler {
	return func(context.Context) (time.Duration, error) {
		tty, err := terminalOf("/proc", os.Getppid(), os.Getenv)
		if err != nil {
			return 0, err
		}
		return ttyIdle(tty, time.Now())
	}
}
