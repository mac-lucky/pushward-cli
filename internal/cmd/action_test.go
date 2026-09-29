package cmd

import (
	"bytes"
	"context"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type manifestEntry struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// TestActionManifest holds action/action.yml to the gha entrypoint: it must
// declare exactly the inputs runGHA reads and the outputs it writes. The
// release copies the file into mac-lucky/pushward-action and pins the image.
func TestActionManifest(t *testing.T) {
	data, err := os.ReadFile("../../action/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Inputs  map[string]manifestEntry `yaml:"inputs"`
		Outputs map[string]manifestEntry `yaml:"outputs"`
		Runs    struct {
			Using string   `yaml:"using"`
			Image string   `yaml:"image"`
			Args  []string `yaml:"args"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	// Record every input a run that reaches the API asks for: that covers
	// inputFlags as well as the inputs read by name.
	f, srv := newFake(t)
	f.reply("PATCH /activities/gha-mac-lucky-demo-42-build", 200, `{"state":"ongoing"}`)
	env, _ := ghaEnv(t, map[string]string{"token": "hlk_a", "command": "activity update", "text": "x"})
	read := map[string]bool{}
	var out bytes.Buffer
	a := &App{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out,
		HTTP:  srv.Client(),
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
		Getenv: func(k string) string {
			if name, ok := strings.CutPrefix(k, "INPUT_"); ok {
				read[strings.ToLower(name)] = true
			}
			if v, ok := env[k]; ok {
				return v
			}
			return os.Getenv(k)
		},
	}
	if code := Execute(a, []string{"gha"}); code != 0 {
		t.Fatalf("gha run: exit %d\n%s", code, out.String())
	}
	compareNames(t, "input", manifest.Inputs, read)

	written := map[string]bool{}
	for _, o := range ghaOutputs(nil, "", "") {
		written[o[0]] = true
	}
	compareNames(t, "output", manifest.Outputs, written)

	for name, in := range manifest.Inputs {
		// GitHub does not enforce required; gha checks the token itself and
		// every other input is optional for some command.
		if in.Required != (name == "token") {
			t.Errorf("input %q: required is %v, only token is required", name, in.Required)
		}
	}

	r := manifest.Runs
	if r.Using != "docker" || !slices.Equal(r.Args, []string{"gha"}) {
		t.Errorf("runs: using %q, args %q; want docker and [gha]", r.Using, r.Args)
	}
	// The release job's sed rewrites the tag after this prefix.
	if !regexp.MustCompile(`^docker://ghcr\.io/mac-lucky/pushward-cli:[^'"]+$`).MatchString(r.Image) {
		t.Errorf("runs.image %q is not docker://ghcr.io/mac-lucky/pushward-cli:<tag>", r.Image)
	}
}

// compareNames reports the names only one side has, and entries without a
// description.
func compareNames(t *testing.T, kind string, declared map[string]manifestEntry, used map[string]bool) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(used)) {
		if _, ok := declared[name]; !ok {
			t.Errorf("gha uses %s %q, but action.yml does not declare it", kind, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !used[name] {
			t.Errorf("action.yml declares %s %q, which gha never uses", kind, name)
		}
		if declared[name].Description == "" {
			t.Errorf("%s %q has no description", kind, name)
		}
	}
}
