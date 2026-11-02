package api

import (
	"errors"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	for n, top := range []time.Duration{2, 4, 8, 16, 30, 30, 30} {
		top *= time.Second
		for range 50 {
			if d := Backoff(n, errors.New("reset")); d <= top*3/4 || d > top {
				t.Fatalf("failure %d: waited %s, want up to a quarter under %s", n, d, top)
			}
		}
	}
	if d := Backoff(1000, nil); d > backoffMax {
		t.Errorf("a long outage waits %s", d)
	}
	// The server asking for longer wins, up to the cap.
	slow := &Error{Status: 429, Code: CodeAnswerWaitLimit, RetryAfterMs: 10_000}
	if d := Backoff(0, slow); d != 10*time.Second {
		t.Errorf("retry_after_ms 10000: waited %s", d)
	}
	slow.RetryAfterMs = 600_000
	if d := Backoff(0, slow); d != backoffMax {
		t.Errorf("retry_after_ms 600000: waited %s", d)
	}
}

func TestTransient(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&TransportError{Err: errors.New("reset")}, true},
		{&Error{Status: 502}, true},
		{&Error{Status: 429, Code: CodeAnswerWaitLimit}, true},
		{&Error{Status: 429, Code: CodeQuotaExceeded}, false},
		{&Error{Status: 401}, false},
		{&Error{Status: 404}, false},
		{errors.New("decoding"), false},
	} {
		if got := Transient(c.err); got != c.want {
			t.Errorf("Transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
