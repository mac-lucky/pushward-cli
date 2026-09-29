package gha

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	if got := Slug("mac-lucky/pushward-cli", "123", "build"); got != "gha-mac-lucky-pushward-cli-123-build" {
		t.Errorf("got %s", got)
	}
	if got := Slug("o/r.name", "1", "job with spaces"); got != "gha-o-r-name-1-job-with-spaces" {
		t.Errorf("got %s", got)
	}
	long := Slug("owner/"+strings.Repeat("r", 150), "1", "a")
	other := Slug("owner/"+strings.Repeat("r", 150), "1", "b")
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	if !valid.MatchString(long) || len(long) > 128 {
		t.Errorf("long slug invalid: %s (%d)", long, len(long))
	}
	if long == other {
		t.Error("truncated slugs of different jobs collide")
	}
}

func TestInput(t *testing.T) {
	env := map[string]string{"INPUT_API-URL": " https://x ", "INPUT_FAIL-ON-ERROR": "false"}
	get := func(k string) string { return env[k] }
	if Input(get, "api-url") != "https://x" || Input(get, "fail-on-error") != "false" {
		t.Error("hyphenated inputs not read")
	}
}

func TestLines(t *testing.T) {
	got := Lines("a=1\n\n  b=2  \r\n")
	if len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Errorf("got %q", got)
	}
}

func TestWriteOutputs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(p, []byte("earlier=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteOutputs(p, [][2]string{{"id", "42"}, {"response", "{\n\"a\": 1\n}"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	re := regexp.MustCompile(`^earlier=1\nid<<(ghadelimiter_[0-9a-f]+)\n42\n(ghadelimiter_[0-9a-f]+)\nresponse<<(ghadelimiter_[0-9a-f]+)\n\{\n"a": 1\n\}\n(ghadelimiter_[0-9a-f]+)\n$`)
	m := re.FindStringSubmatch(string(data))
	if m == nil || m[1] != m[2] || m[3] != m[4] {
		t.Errorf("output file:\n%s", data)
	}
}

func TestEscape(t *testing.T) {
	if got := EscapeData("50%\nnext"); got != "50%25%0Anext" {
		t.Errorf("data: %s", got)
	}
	var b strings.Builder
	Annotate(&b, "error", "bad\nthing")
	if got := b.String(); got != "::error title=PushWard::bad%0Athing\n" {
		t.Errorf("annotation: %q", got)
	}
}
