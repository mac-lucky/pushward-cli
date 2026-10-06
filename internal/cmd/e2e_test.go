package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-cli/internal/e2e"
)

// testE2EKey is key A of the shared vectors; its Key ID is 767c0806.
const testE2EKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func openSent(t *testing.T, key string, body map[string]any) e2e.Message {
	t.Helper()
	k, err := e2e.ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := body["encrypted"].(string)
	m, err := k.Open(env)
	if err != nil {
		t.Fatalf("open %q: %v", env, err)
	}
	for _, f := range sealedFields {
		if _, ok := body[f]; ok {
			t.Errorf("%s sent next to encrypted: %v", f, body)
		}
	}
	return m
}

func TestNotifyEncrypts(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", strings.ToUpper(testE2EKey))
	f.reply("POST /notifications", 201, `{"id":5,"delivery":"all"}`)
	r := runCLI(t, srv, nil, `{"subtitle":"from data","level":"active"}`,
		"notify", "--title", "Rotated", "--body", "new password in the vault", "--url", "https://vault.example.com",
		"--data", "-", "-F", "metadata.env=prod", "--level", "time-sensitive")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	b := f.calls[0].Body
	want := e2e.Message{Title: "Rotated", Subtitle: "from data", Body: "new password in the vault", URL: "https://vault.example.com"}
	if m := openSent(t, testE2EKey, b); m != want {
		t.Errorf("sealed %+v, want %+v", m, want)
	}
	if b["level"] != "time-sensitive" || mustJSON(t, b["metadata"]) != `{"env":"prod"}` {
		t.Errorf("plaintext fields lost: %v", b)
	}
}

func TestNotifyEncryptChoices(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /notifications", 201, `{"id":5}`)
	t.Setenv("PUSHWARD_E2E_KEY", "")

	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--encrypt")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "PUSHWARD_E2E_KEY") || len(f.calls) != 0 {
		t.Errorf("--encrypt without a key: exit %d %q, %d calls", r.code, r.stderr, len(f.calls))
	}
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b")
	if r.code != 0 || f.calls[0].Body["title"] != "a" {
		t.Errorf("no key: exit %d body %v", r.code, f.calls[0].Body)
	}

	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	f.calls = nil
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--no-encrypt")
	if r.code != 0 || f.calls[0].Body["title"] != "a" || f.calls[0].Body["encrypted"] != nil {
		t.Errorf("--no-encrypt: exit %d body %v", r.code, f.calls[0].Body)
	}
	// Sealed elsewhere: passed through untouched.
	r = runCLI(t, srv, nil, "", "notify", "-f", "encrypted=pw1.sealed-elsewhere", "--level", "passive")
	if r.code != 0 || mustJSON(t, f.calls[1].Body) != `{"encrypted":"pw1.sealed-elsewhere","level":"passive"}` {
		t.Errorf("existing encrypted: exit %d body %v", r.code, f.calls[1].Body)
	}

	f.calls = nil
	for name, tc := range map[string]struct {
		argv []string
		want string
	}{
		"both":           {[]string{"--title", "a", "--body", "b", "--encrypt", "--no-encrypt"}, "mutually exclusive"},
		"no body":        {[]string{"--title", "a"}, "body is required"},
		"javascript url": {[]string{"--title", "a", "--body", "b", "--url", "javascript:alert(1)"}, "url scheme"},
		"long title":     {[]string{"--title", strings.Repeat("x", 257), "--body", "b"}, "title is longer"},
		"typed title":    {[]string{"-F", "title=5", "--body", "b"}, "title must be a string"},
		"too long":       {[]string{"--title", "a", "--body", strings.Repeat("x", 3000)}, "too long to encrypt"},
	} {
		r := runCLI(t, srv, nil, "", append([]string{"notify"}, tc.argv...)...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%s: exit %d %q, want %q", name, r.code, r.stderr, tc.want)
		}
	}
	t.Setenv("PUSHWARD_E2E_KEY", "hlk_oops")
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "integration key, not an encryption key") {
		t.Errorf("hlk_ as e2e key: exit %d %q", r.code, r.stderr)
	}
	// A broken key does not get in the way of an explicit opt-out.
	r = runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b", "--no-encrypt")
	if r.code != 0 {
		t.Errorf("--no-encrypt with a broken key: exit %d %q", r.code, r.stderr)
	}
	if len(f.calls) != 1 {
		t.Errorf("refused sends reached the API: %d calls", len(f.calls))
	}
}

func TestScheduleCreateEncrypts(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	f.reply("POST /notifications/scheduled", 201, `{"id":3,"send_at":"2026-09-29T14:00:00Z"}`)
	r := runCLI(t, srv, nil, "", "schedule", "create", "--in", "2h", "--data", `{"title":"Renew","body":"cert expires Friday"}`)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	b := f.calls[0].Body
	if m := openSent(t, testE2EKey, b); m.Title != "Renew" || m.Body != "cert expires Friday" {
		t.Errorf("sealed %+v", m)
	}
	if b["send_at"] != "2026-09-29T14:00:00Z" {
		t.Errorf("send_at %v", b["send_at"])
	}
}

func TestAPINeverEncrypts(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	f.reply("POST /notifications", 201, `{"id":1}`)
	runCLI(t, srv, nil, "", "api", "/notifications", "-f", "title=Hi", "-f", "body=There")
	if got := mustJSON(t, f.calls[0].Body); got != `{"body":"There","title":"Hi"}` {
		t.Errorf("body %s", got)
	}
}

func TestEncryptionUnavailableHint(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	f.reply("POST /notifications", 422, `{"status":422,"code":"notification.encryption_unavailable","detail":"organization keys cannot send encrypted notifications"}`)
	r := runCLI(t, srv, nil, "", "notify", "--title", "a", "--body", "b")
	if r.code != ExitError || !strings.Contains(r.stderr, "pass --no-encrypt") {
		t.Errorf("exit %d %q", r.code, r.stderr)
	}
}

func readConfig(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("PUSHWARD_CONFIG_DIR"), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestE2EKeyLifecycle(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_API_TOKEN", "")
	t.Setenv("PUSHWARD_E2E_KEY", "")
	f.reply("GET /auth/me", 200, `{"id":"u1"}`)

	if r := runCLI(t, srv, nil, "", "e2e", "key-id"); r.code != ExitUsage {
		t.Errorf("key-id without a key: exit %d", r.code)
	}

	r := runCLI(t, srv, nil, "", "e2e", "generate", "--save")
	if r.code != 0 {
		t.Fatalf("generate: exit %d %s", r.code, r.stderr)
	}
	gen := decode([]byte(r.stdout))
	k, err := e2e.ParseKey(str(gen, "key"))
	if err != nil || k.ID() != str(gen, "key_id") {
		t.Fatalf("generated %v: %v", gen, err)
	}
	if c := readConfig(t); c["e2e_key"] != k.Hex() {
		t.Errorf("stored %v", c)
	}

	// login and logout rewrite the file and keep the encryption key.
	if r := runCLI(t, srv, nil, "hlk_new\n", "auth", "login", "--with-token"); r.code != 0 {
		t.Fatalf("login: exit %d %s", r.code, r.stderr)
	}
	if c := readConfig(t); c["e2e_key"] != k.Hex() || c["token"] != "hlk_new" {
		t.Errorf("after login %v", c)
	}
	if r := runCLI(t, srv, nil, "", "auth", "logout"); r.code != 0 {
		t.Fatalf("logout: exit %d", r.code)
	}
	if c := readConfig(t); c["e2e_key"] != k.Hex() || c["token"] != "" {
		t.Errorf("after logout %v", c)
	}

	// A different key is not replaced by accident.
	pasted := "0001 0203 0405 0607 0809 0A0B 0C0D 0E0F\n1011 1213 1415 1617 1819 1A1B 1C1D 1E1F\n"
	if r := runCLI(t, srv, nil, pasted, "e2e", "import"); r.code != ExitUsage || !strings.Contains(r.stderr, "--force") {
		t.Errorf("import over another key: exit %d %q", r.code, r.stderr)
	}
	if r := runCLI(t, srv, nil, "", "e2e", "generate", "--save"); r.code != ExitUsage {
		t.Errorf("generate over another key: exit %d", r.code)
	}
	r = runCLI(t, srv, nil, pasted, "e2e", "import", "--force")
	if r.code != 0 || str(decode([]byte(r.stdout)), "key_id") != "767c0806" {
		t.Fatalf("import --force: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	if c := readConfig(t); c["e2e_key"] != testE2EKey {
		t.Errorf("imported %v", c)
	}
	// The same key again is fine without --force.
	if r := runCLI(t, srv, nil, testE2EKey, "e2e", "import"); r.code != 0 {
		t.Errorf("re-import: exit %d %s", r.code, r.stderr)
	}
	if r := runCLI(t, srv, nil, "hlk_abc\n", "e2e", "import"); r.code != ExitUsage || !strings.Contains(r.stderr, "integration key") {
		t.Errorf("import hlk_: exit %d %q", r.code, r.stderr)
	}

	r = runCLI(t, srv, nil, "", "e2e", "key-id")
	if got := decode([]byte(r.stdout)); r.code != 0 || str(got, "key_id") != "767c0806" || !strings.HasSuffix(str(got, "source"), "config.json") {
		t.Errorf("key-id: exit %d %s", r.code, r.stdout)
	}
	t.Setenv("PUSHWARD_E2E_KEY", strings.Repeat("ff", 32))
	r = runCLI(t, srv, nil, "", "e2e", "key-id")
	if got := decode([]byte(r.stdout)); str(got, "source") != "PUSHWARD_E2E_KEY" || str(got, "key_id") == "767c0806" {
		t.Errorf("env key-id: %s", r.stdout)
	}
	t.Setenv("PUSHWARD_E2E_KEY", "")

	if r := runCLI(t, srv, nil, "", "e2e", "remove"); r.code != 0 {
		t.Errorf("remove: exit %d", r.code)
	}
	if c := readConfig(t); c["e2e_key"] != "" {
		t.Errorf("after remove %v", c)
	}
}

func TestE2EEncryptDecrypt(t *testing.T) {
	_, srv := newFake(t)
	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)

	r := runCLI(t, srv, nil, "", "e2e", "encrypt", "--title", "Disk full", "--body", "/var at 97%", "--url", "myapp://disk")
	if r.code != 0 {
		t.Fatalf("encrypt: exit %d %s", r.code, r.stderr)
	}
	env := str(decode([]byte(r.stdout)), "encrypted")
	if !strings.HasPrefix(env, "pw1.767c0806.") {
		t.Fatalf("envelope %q", r.stdout)
	}
	want := `{"title":"Disk full","body":"/var at 97%","url":"myapp://disk"}`
	// From the encrypt output on stdin, and as the argument.
	for _, tc := range []struct{ stdin, arg string }{{r.stdout, ""}, {"", env}, {env + "\n", "-"}} {
		argv := []string{"e2e", "decrypt"}
		if tc.arg != "" {
			argv = append(argv, tc.arg)
		}
		d := runCLI(t, srv, nil, tc.stdin, argv...)
		if d.code != 0 || strings.TrimSpace(d.stdout) != want {
			t.Errorf("decrypt %v: exit %d %s %s", argv, d.code, d.stdout, d.stderr)
		}
	}

	r = runCLI(t, srv, nil, `{"title":"x","body":"y","level":"critical"}`, "e2e", "encrypt", "--jq", ".encrypted")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "pw1.767c0806.") {
		t.Errorf("encrypt from stdin: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	for name, stdin := range map[string]string{"empty": "", "array": `["x"]`, "no body": `{"title":"x"}`} {
		if r := runCLI(t, srv, nil, stdin, "e2e", "encrypt"); r.code != ExitUsage {
			t.Errorf("encrypt %s: exit %d", name, r.code)
		}
	}

	t.Setenv("PUSHWARD_E2E_KEY", strings.Repeat("ab", 32))
	if r := runCLI(t, srv, nil, "", "e2e", "decrypt", env); r.code != ExitError || !strings.Contains(r.stderr, "key ID 767c0806") {
		t.Errorf("wrong key: exit %d %q", r.code, r.stderr)
	}
	if r := runCLI(t, srv, nil, `{"title":"x"}`, "e2e", "decrypt"); r.code != ExitUsage {
		t.Errorf("JSON without encrypted: exit %d", r.code)
	}
}
