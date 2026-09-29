package body

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyFieldPaths(t *testing.T) {
	obj := map[string]any{}
	for _, f := range []string{
		"content.progress=0.5",
		"content.step_labels[]=Build",
		"content.step_labels[]=Test",
		"actions[0].id=ok",
		"actions[0].title=OK",
		"actions[1].id=no",
		`metadata.a\.b=dotted`,
		"push=false",
		"volume=null",
		"content.value={\"cpu\":42}",
		"code=007",
	} {
		if err := ApplyField(obj, f, true, nil); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	got := mustJSON(t, obj)
	want := `{"actions":[{"id":"ok","title":"OK"},{"id":"no"}],"code":"007","content":{"progress":0.5,"step_labels":["Build","Test"],"value":{"cpu":42}},"metadata":{"a.b":"dotted"},"push":false,"volume":null}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestStringFieldIsNeverTyped(t *testing.T) {
	obj := map[string]any{}
	for _, f := range []string{"a=true", "b=12", "c={}", "d=@nope"} {
		if err := ApplyField(obj, f, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustJSON(t, obj); got != `{"a":"true","b":"12","c":"{}","d":"@nope"}` {
		t.Errorf("got %s", got)
	}
}

func TestTypedFileAndStdin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(p, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{}
	if err := ApplyField(obj, "body=@"+p, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := ApplyField(obj, "text=@-", true, strings.NewReader("from stdin")); err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, obj); got != `{"body":"from file","text":"from stdin"}` {
		t.Errorf("got %s", got)
	}
}

func TestApplyFieldErrors(t *testing.T) {
	for _, tc := range []struct{ field, want string }{
		{"novalue", "want key=value"},
		{"=x", "want key=value"},
		{"a..b=1", "empty key"},
		{"a[x]=1", "bad index"},
		{"a[2]=1", "past the end"},
		{"a[=1", "unterminated"},
		{"a={bad", "invalid JSON"},
	} {
		err := ApplyField(map[string]any{}, tc.field, true, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.field, err, tc.want)
		}
	}
}

func TestPathConflict(t *testing.T) {
	obj := map[string]any{}
	if err := ApplyField(obj, "title=x", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := ApplyField(obj, "title.sub=y", true, nil); err == nil {
		t.Error("setting a key under a string should fail")
	}
}

func TestParseJSON(t *testing.T) {
	obj, err := ParseJSON(`{"n": 12345678901234567890}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Numbers survive untouched instead of rounding through float64.
	if got := mustJSON(t, obj); got != `{"n":12345678901234567890}` {
		t.Errorf("got %s", got)
	}
	if _, err := ParseJSON(`[1]`, nil); err == nil {
		t.Error("an array body should be rejected")
	}
	if _, err := ParseJSON(`{} {}`, nil); err == nil {
		t.Error("trailing data should be rejected")
	}
	obj, err = ParseJSON("-", strings.NewReader(`{"a":1}`))
	if err != nil || mustJSON(t, obj) != `{"a":1}` {
		t.Errorf("stdin: %v %v", obj, err)
	}
}

func TestMerge(t *testing.T) {
	dst := map[string]any{"content": map[string]any{"template": "generic", "progress": 0.1}, "tags": []any{"a"}}
	Merge(dst, map[string]any{"content": map[string]any{"progress": 0.9}, "tags": []any{"b"}, "state": "ongoing"})
	want := `{"content":{"progress":0.9,"template":"generic"},"state":"ongoing","tags":["b"]}`
	if got := mustJSON(t, dst); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestReadCapsInput(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", MaxInput+1))
	if _, _, err := Read("-", big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized stdin accepted: %v", err)
	}
	if _, ok, _ := Read("plain", nil); ok {
		t.Error("a literal argument was treated as a reference")
	}
}
