//go:build unix

package cmd

import (
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Ctrl-C during an export saves nothing and removes the temporary file,
// which holds members' addresses.
func TestOrgAuditExportInterrupted(t *testing.T) {
	// The test process takes SIGINT too, so it lives whatever the export does.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	srv, _ := exportServer(t, func(w http.ResponseWriter) {
		io.WriteString(w, "id,time\n")
		w.(http.Flusher).Flush()
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
		// Keep the download going until the client hangs up, or long enough
		// for it to finish if the interrupt went unnoticed.
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if _, err := io.WriteString(w, "1,2026-01-01T00:00:00Z\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	dir := t.TempDir()
	r := runCLI(t, srv, nil, "", "org", "audit", "export", "-o", filepath.Join(dir, "audit.csv"))
	if entries, _ := os.ReadDir(dir); r.code == 0 || len(entries) != 0 {
		t.Errorf("interrupted: exit %d, %d files left (stderr %q)", r.code, len(entries), r.stderr)
	}
}
