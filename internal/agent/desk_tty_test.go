//go:build linux || darwin

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The age of a terminal is its access time, which reading a key sets; its
// modification time, which output sets, does not count.
func TestTTYIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, c := range []struct {
		atime, mtime time.Time
		want         time.Duration
	}{
		{now.Add(-2 * time.Minute), now, 2 * time.Minute},
		{now.Add(-3 * time.Second), now.Add(-time.Hour), 3 * time.Second},
		// A clock that moved back: no negative age.
		{now.Add(time.Minute), now, 0},
	} {
		if err := os.Chtimes(path, c.atime, c.mtime); err != nil {
			t.Fatal(err)
		}
		got, err := ttyIdle(path, now)
		if err != nil || got < c.want-time.Second || got > c.want+time.Second {
			t.Errorf("atime %s ago: idle %s, %v; want %s", now.Sub(c.atime), got, err, c.want)
		}
	}
	if _, err := ttyIdle(filepath.Join(t.TempDir(), "gone"), now); err == nil {
		t.Error("a terminal that is not there has an idle time")
	}
}
