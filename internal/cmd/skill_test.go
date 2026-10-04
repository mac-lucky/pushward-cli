package cmd

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kballard/go-shellquote"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// TestSkillCommands holds the agent skill under skills/ to the command tree.
// Agents run its examples as written, so a renamed command or flag would fail
// on someone else's machine with a usage error nobody connects to the docs.
//
// Every pushward invocation in a code block is parsed the way the CLI parses
// it: the command must exist, every flag must parse (so --wait 600 fails for
// its missing unit), and the positional arguments must pass the command's
// own checks. Code spans in prose are fragments, so they only need to name a
// real command and flags that exist on it.
func TestSkillCommands(t *testing.T) {
	allFlags := map[string]bool{}
	walk(NewRoot(NewApp("test", "", "")), func(c *cobra.Command) {
		for _, set := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			set.VisitAll(func(f *pflag.Flag) {
				allFlags["--"+f.Name] = true
				if f.Shorthand != "" {
					allFlags["-"+f.Shorthand] = true
				}
			})
		}
	})

	const dir = "../../skills"
	var docs []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			docs = append(docs, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatalf("no skill files under %s", dir)
	}

	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		name, _ := filepath.Rel(dir, path)
		if filepath.Base(path) == "SKILL.md" {
			checkFrontmatter(t, name, filepath.Base(filepath.Dir(path)), text)
		}
		checkLinks(t, name, filepath.Dir(path), text)

		blocks, spans := codeOf(text)
		n := 0
		for _, block := range blocks {
			invs, err := invocations(block)
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			for _, inv := range invs {
				n++
				if err := runInvocation(inv); err != "" {
					t.Errorf("%s: pushward %s: %s", name, strings.Join(inv, " "), err)
				}
			}
		}
		for _, s := range spans {
			fields := strings.Fields(s)
			if len(fields) == 0 {
				continue
			}
			// A bare flag in prose (`--stale-ttl`) has no command to check
			// against, but it still has to exist somewhere.
			if f := flagName(fields[0]); f != "" && !allFlags[f] {
				t.Errorf("%s: `%s`: no command has %s", name, s, f)
			}
			if !strings.Contains(s, "pushward ") {
				continue
			}
			invs, err := invocations(s)
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			for _, inv := range invs {
				n++
				if err := checkFragment(inv); err != "" {
					t.Errorf("%s: `pushward %s`: %s", name, strings.Join(inv, " "), err)
				}
			}
		}
		if n == 0 {
			t.Errorf("%s: found no pushward invocations; is the parser still matching?", name)
		}
	}
}

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// checkFrontmatter applies the Agent Skills rules that skill installers
// enforce: the name matches the directory and the description fits.
func checkFrontmatter(t *testing.T, file, dirName, text string) {
	t.Helper()
	rest, ok := strings.CutPrefix(text, "---\n")
	head, _, ok2 := strings.Cut(rest, "\n---\n")
	if !ok || !ok2 {
		t.Errorf("%s: no frontmatter", file)
		return
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		t.Errorf("%s: frontmatter: %v", file, err)
		return
	}
	if fm.Name != dirName || !skillName.MatchString(fm.Name) || len(fm.Name) > 64 {
		t.Errorf("%s: name %q must be the directory name %q, lowercase words joined by hyphens, at most 64 characters", file, fm.Name, dirName)
	}
	if fm.Description == "" || len(fm.Description) > 1024 {
		t.Errorf("%s: description is %d characters, want 1 to 1024", file, len(fm.Description))
	}
	if strings.ContainsAny(fm.Description, "<>") {
		t.Errorf("%s: description must not contain angle brackets", file)
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func checkLinks(t *testing.T, file, base, text string) {
	t.Helper()
	for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
		target, _, _ := strings.Cut(m[1], "#")
		if strings.Contains(target, "://") || target == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, target)); err != nil {
			t.Errorf("%s: link %s: %v", file, m[1], err)
		}
	}
}

var codeSpan = regexp.MustCompile("`([^`\n]+)`")

// codeOf returns the fenced blocks, with backslash continuations joined and
// comment lines dropped, and the inline code spans outside them.
func codeOf(text string) (blocks, spans []string) {
	var prose, block strings.Builder
	in := false
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if in {
				blocks = append(blocks, strings.ReplaceAll(block.String(), "\\\n", " "))
				block.Reset()
			}
			in = !in
			continue
		}
		switch {
		case in && strings.HasPrefix(strings.TrimSpace(line), "#"):
		case in:
			block.WriteString(line)
			block.WriteByte('\n')
		default:
			prose.WriteString(line)
			prose.WriteByte('\n')
		}
	}
	for _, m := range codeSpan.FindAllStringSubmatch(prose.String(), -1) {
		spans = append(spans, m[1])
	}
	return blocks, spans
}

func isSeparator(tok string) bool {
	return tok == "|" || tok == ";" || tok == "&&" || tok == "||"
}

// invocations splits shell text into the argument lists of each pushward
// call: at the start of a line, after |, ;, && or ||, and inside $(...),
// quoted or not. Lines that never mention pushward are not split at all.
func invocations(code string) ([][]string, error) {
	var out [][]string
	for line := range strings.SplitSeq(code, "\n") {
		if !strings.Contains(line, "pushward ") {
			continue
		}
		words, err := shellWords(line)
		if err != nil {
			return out, err
		}
		var cur []string
		on := false
		nested := 0 // $( opened inside the current invocation
		for _, tok := range words {
			switch {
			case on && isSeparator(tok):
				out = append(out, cur)
				cur, on = nil, false
				continue
			case !on && (tok == "pushward" || strings.HasSuffix(tok, "$(pushward")):
				cur, on, nested = []string{}, true, 0
				continue
			case !on:
				continue
			}
			nested += strings.Count(tok, "$(")
			trimmed := strings.TrimRight(tok, ")")
			if closes := len(tok) - len(trimmed); closes > nested {
				if trimmed != "" {
					cur = append(cur, trimmed)
				}
				out = append(out, cur)
				cur, on = nil, false
				continue
			} else {
				nested -= closes
			}
			cur = append(cur, tok)
		}
		if on {
			out = append(out, cur)
		}
	}
	return out, nil
}

// shellWords splits a line like a shell would, then splits again any word
// that holds a whole quoted "$(pushward ...)" so the call inside is seen.
func shellWords(line string) ([]string, error) {
	words, err := shellquote.Split(line)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", line, err)
	}
	var out []string
	for _, w := range words {
		before, after, ok := strings.Cut(w, "$(pushward ")
		if !ok {
			out = append(out, w)
			continue
		}
		inner, err := shellquote.Split(after)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", w, err)
		}
		out = append(out, before+"$(pushward")
		out = append(out, inner...)
	}
	return out, nil
}

// runInvocation parses a full example with a fresh command tree, the way the
// CLI would, and runs the command's argument checks. Placeholders such as
// <id> and shell variables such as "$slug" stand in for valid values.
func runInvocation(args []string) string {
	root := NewRoot(NewApp("test", "", ""))
	cmd, rest, err := root.Find(args)
	if err != nil {
		return err.Error()
	}
	if err := cmd.ParseFlags(rest); err != nil {
		return cmd.CommandPath() + ": " + err.Error()
	}
	pos := cmd.Flags().Args()
	if cmd.HasSubCommands() {
		if len(pos) > 0 {
			return "unknown command " + pos[0] + " under " + cmd.CommandPath()
		}
		return cmd.CommandPath() + " needs a subcommand"
	}
	names := positionals(cmd)
	for i, a := range pos {
		if strings.HasPrefix(a, "<") || strings.HasPrefix(a, "$") {
			name := ""
			if i < len(names) {
				name = names[i]
			}
			pos[i] = cmp.Or(placeholderValue[name], "x")
		}
	}
	if cmd.Args != nil {
		if err := cmd.Args(cmd, pos); err != nil {
			return err.Error()
		}
	}
	return ""
}

var placeholderValue = map[string]string{
	"slug":   "x1",
	"id":     "1",
	"key-id": "00000000-0000-0000-0000-000000000000",
}

// checkFragment resolves the leading words of a prose fragment to a command
// and looks up every flag on it, without requiring values or arguments.
func checkFragment(args []string) string {
	cmd := NewRoot(NewApp("test", "", ""))
	i := 0
	for ; i < len(args); i++ {
		sub := findSub(cmd, args[i])
		if sub == nil {
			break
		}
		cmd = sub
	}
	if cmd.HasSubCommands() && i < len(args) && !strings.HasPrefix(args[i], "-") && !strings.HasPrefix(args[i], "<") {
		return "unknown command " + args[i] + " under " + cmd.CommandPath()
	}
	for _, a := range args[i:] {
		f := flagName(a)
		if f == "" || f == "--help" || f == "-h" {
			continue
		}
		var found *pflag.Flag
		for _, set := range []*pflag.FlagSet{cmd.Flags(), cmd.PersistentFlags(), cmd.InheritedFlags()} {
			if name, ok := strings.CutPrefix(f, "--"); ok {
				found = cmp.Or(found, set.Lookup(name))
			} else {
				found = cmp.Or(found, set.ShorthandLookup(f[1:]))
			}
		}
		if found == nil {
			return cmd.CommandPath() + " has no flag " + f
		}
	}
	return ""
}

func findSub(c *cobra.Command, name string) *cobra.Command {
	for _, sub := range c.Commands() {
		if sub.Name() == name || sub.HasAlias(name) {
			return sub
		}
	}
	return nil
}

var flagToken = regexp.MustCompile(`^(--[a-z][a-z0-9-]*|-[a-zA-Z])(=|$)`)

// flagName returns the flag a token names (--state for --state=ended), or ""
// when the token is not a flag.
func flagName(tok string) string {
	m := flagToken.FindStringSubmatch(tok)
	if m == nil {
		return ""
	}
	return m[1]
}
