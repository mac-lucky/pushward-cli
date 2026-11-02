//go:build darwin

package agent

import (
	"context"
	"os/exec"
	"time"
)

// SystemIdle reads the Mac's idle time. macOS keeps the time since the last
// key, click or touch as IOHIDSystem's HIDIdleTime, and ioreg prints it,
// which spares an IOKit binding and cgo.
func SystemIdle() Idler {
	return func(ctx context.Context) (time.Duration, error) {
		out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem", "-r", "-k", "HIDIdleTime", "-d", "1").Output()
		if err != nil {
			return 0, err
		}
		return parseHIDIdle(out)
	}
}
