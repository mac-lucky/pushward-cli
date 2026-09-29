package config

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestWriteIsPrivate(t *testing.T) {
	dir := t.TempDir() + "/nested"
	t.Setenv(EnvConfig, dir)
	p, err := Write(File{Token: "hlk_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode %o, want 600", perm)
	}
	dinfo, _ := os.Stat(dir)
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode %o, want 700", perm)
	}
}

func TestLoadPrecedence(t *testing.T) {
	t.Setenv(EnvConfig, t.TempDir())
	t.Setenv(EnvToken, "")
	t.Setenv(EnvURL, "")
	if _, err := Write(File{Token: "hlk_file", APIURL: "https://file.example"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "hlk_file" || c.APIURL != "https://file.example" || !strings.HasSuffix(c.TokenSource, "config.json") {
		t.Errorf("file: %+v", c)
	}
	t.Setenv(EnvToken, " hlk_env\n")
	t.Setenv(EnvURL, "https://env.example/")
	c, _ = Load("")
	if c.Token != "hlk_env" || c.TokenSource != "env" || c.APIURL != "https://env.example" {
		t.Errorf("env: %+v", c)
	}
	c, _ = Load("http://localhost:8080")
	if c.APIURL != "http://localhost:8080" {
		t.Errorf("flag: %+v", c)
	}
}

func TestLoadWarnsOnOpenFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permissions")
	}
	t.Setenv(EnvConfig, t.TempDir())
	t.Setenv(EnvToken, "")
	p, _ := Write(File{Token: "hlk_x"})
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := Load("")
	if !strings.Contains(c.Warning, "chmod 600") {
		t.Errorf("warning = %q", c.Warning)
	}
}

func TestValidateURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://api.pushward.app":  true,
		"http://localhost:8080":     true,
		"http://127.0.0.1:8080":     true,
		"http://[::1]:8080":         true,
		"http://api.pushward.app":   false,
		"http://10.0.0.5":           false,
		"ftp://api.pushward.app":    false,
		"api.pushward.app":          false,
		"https://api.pushward.app/": true,
	} {
		if _, err := ValidateURL(raw); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", raw, err, ok)
		}
	}
}
