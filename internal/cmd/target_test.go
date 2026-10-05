package cmd

import (
	"strings"
	"testing"
)

// The --target-* flags put an organization target on every send, and
// --no-target clears an activity's.
func TestTargetFlags(t *testing.T) {
	const target = `{"groups":["oncall","sre"],"members":["u-1"],"tags":["wall"]}`
	flags := []string{"--target-groups", "oncall,sre", "--target-tags", "wall", "--target-members", "u-1"}
	for _, tc := range []struct {
		route, reply string
		argv         []string
	}{
		{"POST /notifications", `{"id":7,"delivery":"all"}`, []string{"notify", "--title", "t", "--body", "b"}},
		{"POST /notifications", `{"id":7,"delivery":"all"}`, []string{"notification", "send", "--title", "t", "--body", "b"}},
		{"POST /notifications/scheduled", `{"id":3,"status":"scheduled"}`, []string{"schedule", "create", "--in", "2h", "--title", "t", "--body", "b"}},
		{"POST /activities", `{"slug":"deploy","state":"ended"}`, []string{"activity", "create", "deploy"}},
		{"PATCH /activities/deploy", `{"slug":"deploy","state":"ongoing"}`, []string{"activity", "update", "deploy"}},
	} {
		f, srv := newFake(t)
		f.reply(tc.route, 201, tc.reply)
		r := runCLI(t, srv, nil, "", append(tc.argv, flags...)...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
		}
		if got := mustJSON(t, f.calls[0].Body["target"]); got != target {
			t.Errorf("%v: target %s, want %s", tc.argv, got, target)
		}
	}
}

// activity start sends the target with the create, which a restart of an
// existing slug also replaces it through; the update carries only content.
func TestActivityStartTarget(t *testing.T) {
	f, srv := newFake(t)
	f.reply("POST /activities", 201, `{"slug":"deploy","state":"ended"}`)
	f.reply("PATCH /activities/deploy", 200, `{"slug":"deploy","state":"ongoing"}`)
	r := runCLI(t, srv, nil, "", "activity", "start", "deploy", "--text", "Building", "--target-groups", "oncall")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := mustJSON(t, f.calls[0].Body); got != `{"name":"deploy","slug":"deploy","target":{"groups":["oncall"]}}` {
		t.Errorf("create body %s", got)
	}
	if _, ok := f.calls[1].Body["target"]; ok {
		t.Errorf("update body carries the target: %v", f.calls[1].Body)
	}
}

func TestActivityUpdateNoTarget(t *testing.T) {
	f, srv := newFake(t)
	f.reply("PATCH /activities/deploy", 200, `{"slug":"deploy","state":"ongoing"}`)
	r := runCLI(t, srv, nil, "", "activity", "update", "deploy", "--no-target")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := mustJSON(t, f.calls[0].Body); got != `{"target":null}` {
		t.Errorf("body %s, want the target cleared", got)
	}
	r = runCLI(t, srv, nil, "", "activity", "update", "deploy", "--no-target", "--target-tags", "wall")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--no-target") || len(f.calls) != 1 {
		t.Errorf("--no-target with --target-tags: exit %d %q, %d calls", r.code, r.stderr, len(f.calls))
	}
}

func TestTargetFlagsRefused(t *testing.T) {
	f, srv := newFake(t)
	for _, argv := range [][]string{
		{"notify", "--title", "t", "--body", "b", "--target-groups", "oncall,"},
		{"notify", "--title", "t", "--body", "b", "--target-groups", ""},
		{"activity", "start", "deploy", "--target-groups", "oncall", "-F", "target.tags[]=wall"},
	} {
		if r := runCLI(t, srv, nil, "", argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d %q, want a usage error", argv, r.code, r.stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("%d requests sent, want none", len(f.calls))
	}
}

// A blank step label stays allowed: it leaves that step unlabelled.
func TestStepLabelsKeepBlanks(t *testing.T) {
	f, srv := newFake(t)
	f.reply("PATCH /activities/deploy", 200, `{"slug":"deploy","state":"ongoing"}`)
	r := runCLI(t, srv, nil, "", "activity", "update", "deploy", "--step", "2/3", "--step-labels", "Build,,Ship")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := mustJSON(t, f.calls[0].Body["content"].(map[string]any)["step_labels"]); got != `["Build","","Ship"]` {
		t.Errorf("step_labels %s", got)
	}
}
