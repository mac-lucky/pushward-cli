package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A key that passed the check is not checked again for an hour, by any
// hook process; a refusal is checked again every time.
func TestKeyCheckIsRemembered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pushward", "agent-key-check.json")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeAPI()
	check := func(apiURL, key string) error {
		t.Helper()
		r, _ := newRunner(f, away())
		r.KeyCache = NewKeyCache(path, apiURL, key)
		r.KeyCache.Now = func() time.Time { return now }
		return r.CheckKey(context.Background())
	}
	const apiURL = "https://api.pushward.test"

	for i := range 3 {
		if err := check(apiURL, testToken); err != nil || f.probes != 1 {
			t.Fatalf("run %d: %v, %d key checks", i, err, f.probes)
		}
		now = now.Add(20 * time.Minute)
	}
	// An hour after the check it is asked again.
	if err := check(apiURL, testToken); err != nil || f.probes != 2 {
		t.Errorf("after the hour: %v, %d key checks", err, f.probes)
	}
	// Another key, or the same key against another API, is its own entry.
	if check(apiURL, "hlk_other_key"); f.probes != 3 {
		t.Errorf("another key: %d key checks", f.probes)
	}
	if check("https://api.example.test", testToken); f.probes != 4 {
		t.Errorf("another API: %d key checks", f.probes)
	}
	if check(apiURL, testToken); f.probes != 4 {
		t.Errorf("the first key again: %d key checks", f.probes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{testToken, "hlk_other_key", "pushward.test"} {
		if strings.Contains(string(data), s) {
			t.Errorf("the cache holds %q: %s", s, data)
		}
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", info.Mode().Perm())
		}
	}

	// A clock set back past the check does not count as within the hour.
	now = now.Add(-2 * time.Hour)
	if check(apiURL, testToken); f.probes != 5 {
		t.Errorf("clock set back: %d key checks", f.probes)
	}

	// An organization's key is refused every time and never written down.
	org := newFakeAPI()
	org.org = true
	orgPath := filepath.Join(t.TempDir(), "agent-key-check.json")
	for i := range 2 {
		r, _ := newRunner(org, away())
		r.KeyCache = NewKeyCache(orgPath, apiURL, testToken)
		if err := r.CheckKey(context.Background()); err != ErrOrgKey || org.probes != i+1 {
			t.Errorf("organization key, run %d: %v, %d key checks", i, err, org.probes)
		}
	}
	if _, err := os.Stat(orgPath); err == nil {
		t.Error("a refused key was written down")
	}
}
