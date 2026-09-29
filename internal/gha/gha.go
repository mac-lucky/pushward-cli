// Package gha holds the GitHub Actions plumbing for `pushward gha`: reading
// inputs, the workflow-command escapes, default slugs and step outputs.
package gha

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Input reads an action input the way the runner exports it: INPUT_ plus the
// upper-cased name, spaces as underscores and hyphens kept (INPUT_API-URL).
func Input(getenv func(string) string, name string) string {
	return strings.TrimSpace(getenv("INPUT_" + strings.ToUpper(strings.ReplaceAll(name, " ", "_"))))
}

// Lines splits a multi-line input into its non-empty, trimmed lines.
func Lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// Slug is the default activity slug for a job: gha-<owner>-<repo>-<run>-<job>.
// Re-runs of the same run reuse it, so a retried job updates its card
// instead of stacking a second one. Past the API's 128-character limit the
// tail is replaced by a hash of the whole string, keeping slugs unique.
func Slug(repository, runID, job string) string {
	raw := "gha-" + strings.Join([]string{repository, runID, job}, "-")
	s := strings.Trim(slugUnsafe.ReplaceAllString(raw, "-"), "-")
	if len(s) <= 128 {
		return s
	}
	sum := sha256.Sum256([]byte(raw))
	return strings.TrimRight(s[:121], "-") + "-" + hex.EncodeToString(sum[:3])
}

// RunURL links to the current workflow run.
func RunURL(getenv func(string) string) string {
	server, repo, run := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || run == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, run)
}

// EscapeData escapes a workflow command's message.
func EscapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// Mask asks the runner to redact value from the rest of the job log.
func Mask(w io.Writer, value string) {
	fmt.Fprintf(w, "::add-mask::%s\n", value)
}

// Annotate writes a PushWard warning or error annotation for the run page.
func Annotate(w io.Writer, level, msg string) {
	fmt.Fprintf(w, "::%s title=PushWard::%s\n", level, EscapeData(msg))
}

// WriteOutputs appends step outputs to the $GITHUB_OUTPUT file using the
// heredoc form, with a random delimiter so no value can end its own block.
func WriteOutputs(path string, outputs [][2]string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) // #nosec G304 -- the runner names this file
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, o := range outputs {
		delim, err := delimiter(o[1])
		if err != nil {
			_ = f.Close()
			return err
		}
		fmt.Fprintf(&b, "%s<<%s\n%s\n%s\n", o[0], delim, o[1], delim)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func delimiter(value string) (string, error) {
	for range 5 {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		d := "ghadelimiter_" + hex.EncodeToString(buf)
		if !strings.Contains(value, d) {
			return d, nil
		}
	}
	return "", fmt.Errorf("could not pick an output delimiter")
}
