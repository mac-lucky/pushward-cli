//go:build unix

package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Closing the terminal tab sends the hook SIGHUP while a card waits: it ends
// the card and exits 0 with nothing on stdout, so the prompt shows in the
// terminal if there still is one.
func TestAgentHookHangup(t *testing.T) {
	f, srv := newFake(t)
	slug := agentSlug("s1", "Bash", `{"command":"make deploy"}`)
	createRoute(f, false, 201, `{"slug":"`+slug+`","state":"ongoing","content":{}}`)
	f.reply("PATCH /activities/"+slug, 200, `{"slug":"`+slug+`","state":"ongoing"}`)
	waiting := make(chan struct{})
	var once sync.Once
	f.routes["GET /activities/"+slug] = func(call) (int, string) {
		once.Do(func() { close(waiting) })
		return 200, `{"slug":"` + slug + `","state":"ongoing","devices_targeted":1,"devices_started":1,"content":{"template":"approval"}}`
	}
	// The test process takes SIGHUP too, so it lives whatever the hook does.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- runApp(t, srv, nil, hookPermission, func(a *App) { a.Context = ctx }, "agent", "hook", "claude", "--desk", "away")
	}()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never read the card")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook kept waiting after SIGHUP")
	}
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ended := false
	for _, c := range f.calls {
		ended = ended || (c.Method == "PATCH" && c.Path == "/activities/"+slug && c.Body["state"] == "ended")
	}
	if !ended {
		t.Errorf("the card was left up: %+v", f.calls)
	}
}
