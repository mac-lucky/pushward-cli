//go:build linux || darwin

package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// ttyIdle is how long ago the terminal at path last read a key. Reading
// input sets a terminal's access time and writing output does not. Linux
// moves it in steps of about 8s, but the first key after a long idle moves
// it at once, which is the one that counts.
func ttyIdle(path string, now time.Time) (time.Duration, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	sec, nsec := st.Atim.Unix()
	return max(now.Sub(time.Unix(sec, nsec)), 0), nil
}
