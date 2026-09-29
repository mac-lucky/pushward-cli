package cmd

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/kballard/go-shellquote"
	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/gha"
)

// inputFlags maps convenience inputs of the Action to CLI flags. An input
// goes to the first listed flag the chosen command has; the Action warns
// when a set input has nowhere to go.
var inputFlags = []struct {
	input string
	flags []string
	multi bool
}{
	{"title", []string{"title"}, false},
	{"body", []string{"body"}, false},
	{"subtitle", []string{"subtitle"}, false},
	{"level", []string{"level"}, false},
	{"url", []string{"url"}, false},
	{"name", []string{"name"}, false},
	{"template", []string{"template"}, false},
	{"text", []string{"text"}, false},
	{"progress", []string{"progress"}, false},
	{"icon", []string{"icon"}, false},
	{"color", []string{"color"}, false},
	{"status", []string{"status"}, false},
	{"wait", []string{"wait", "timeout"}, false},
	{"actions", []string{"action", "option"}, true},
	{"fields", []string{"raw-field"}, true},
	{"json", []string{"data"}, false},
}

func newGHACmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:                "gha",
		Short:              "Entrypoint of the pushward GitHub Action",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runGHA()
		},
	}
}

// runGHA turns the Action's inputs into one CLI invocation, runs it, and
// reports the result as step outputs and annotations.
func (a *App) runGHA() error {
	in := func(name string) string { return gha.Input(a.Getenv, name) }
	token := in("token")
	if token != "" {
		fmt.Fprintf(a.Stdout, "::add-mask::%s\n", token)
	}
	failOnError := in("fail-on-error") != "false"

	argv, slug, err := a.ghaArgs(in)
	if err == nil && token == "" {
		err = usagef("the token input is required: pass an integration key from a secret")
	}
	if err != nil {
		return a.ghaFail(err, failOnError)
	}
	fmt.Fprintf(a.Stdout, "::debug::pushward %s\n", gha.EscapeData(strings.Join(argv, " ")))

	var out bytes.Buffer
	run := &App{
		Version: a.Version, Commit: a.Commit, Date: a.Date,
		Stdin: a.Stdin, Stdout: &out, Stderr: a.Stderr,
		HTTP: a.HTTP, Now: a.Now, Sleep: a.Sleep, Getenv: a.Getenv,
		token: token, apiURL: in("api-url"),
	}
	root := NewRoot(run)
	root.SetArgs(argv)
	runErr := root.Execute()

	if out.Len() > 0 {
		fmt.Fprintf(a.Stdout, "::group::PushWard response\n%s", out.String())
		if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			fmt.Fprintln(a.Stdout)
		}
		fmt.Fprintln(a.Stdout, "::endgroup::")
	}
	if err := gha.WriteOutputs(a.Getenv("GITHUB_OUTPUT"), ghaOutputs(run.Body, run.Status, slug)); err != nil {
		gha.Annotate(a.Stdout, "warning", "writing step outputs: "+err.Error())
	}
	if runErr != nil {
		return a.ghaFail(runErr, failOnError)
	}
	return nil
}

func (a *App) ghaFail(err error, failOnError bool) error {
	if !failOnError {
		gha.Annotate(a.Stdout, "warning", err.Error())
		return nil
	}
	gha.Annotate(a.Stdout, "error", err.Error())
	return &reportedError{err}
}

// ghaArgs builds the argument list from the command input plus the
// convenience inputs, then fills GitHub-context defaults for anything the
// user left unset.
func (a *App) ghaArgs(in func(string) string) (argv []string, slug string, err error) {
	command := in("command")
	if command == "" {
		command = "notify"
	}
	words, err := shellquote.Split(command)
	if err != nil {
		return nil, "", usagef("command input: %v", err)
	}
	if len(words) == 0 {
		return nil, "", usagef("command input is empty")
	}
	switch words[0] {
	case "auth", "completion", "gha", "help", "version":
		return nil, "", usagef("command %q is not available in the Action", words[0])
	}

	// Resolve and parse on a throwaway tree: parsing marks flags as changed,
	// and the real run needs a clean one.
	probe := NewRoot(&App{Getenv: a.Getenv})
	target, _, err := probe.Find(words)
	if err != nil || target == probe || target.HasSubCommands() {
		return nil, "", usagef("command input: unknown command %q", command)
	}

	argv = slices.Clone(words)
	for _, m := range inputFlags {
		val := in(m.input)
		if val == "" {
			continue
		}
		flag := ""
		for _, f := range m.flags {
			if target.Flags().Lookup(f) != nil {
				flag = f
				break
			}
		}
		if flag == "" {
			gha.Annotate(a.Stdout, "warning", fmt.Sprintf("input %q is ignored by %q", m.input, target.CommandPath()))
			continue
		}
		if m.multi {
			for _, l := range gha.Lines(val) {
				argv = append(argv, "--"+flag+"="+l)
			}
			continue
		}
		argv = append(argv, "--"+flag+"="+val)
	}

	// The API URL only comes from its own input: a flag smuggled into the
	// command through an interpolated value must not redirect the key.
	_, rest, _ := probe.Find(argv)
	if err := target.ParseFlags(rest); err != nil {
		return nil, "", usagef("command input: %v", err)
	}
	if f := target.Flags().Lookup("api-url"); f != nil && f.Changed {
		return nil, "", usagef("set the API URL with the api-url input, not in command")
	}

	env := a.Getenv
	def := func(flag, value string) {
		if value == "" {
			return
		}
		if f := target.Flags().Lookup(flag); f != nil && !f.Changed {
			argv = append(argv, "--"+flag+"="+value)
		}
	}
	repo := env("GITHUB_REPOSITORY")
	_, repoName, _ := strings.Cut(repo, "/")
	runURL := gha.RunURL(env)
	group := target.Parent().Name()
	switch {
	case group == "activity" && target.Name() == "start":
		def("name", env("GITHUB_WORKFLOW"))
		if repoName != "" && env("GITHUB_JOB") != "" {
			def("subtitle", repoName+" / "+env("GITHUB_JOB"))
		}
		def("url", runURL)
		// A hosted job lasts at most 6h: past that nobody will end the card.
		def("stale-ttl", "6h")
		def("ended-ttl", "15m")
	case group == "activity" && target.Name() == "create":
		def("name", env("GITHUB_WORKFLOW"))
	case group == "activity" && target.Name() == "end":
		// The start step may never have run, e.g. when an earlier step failed.
		def("ignore-missing", "true")
	case target.Annotations["operation"] == "createNotification":
		def("url", runURL)
		def("thread", repo)
		def("source", "github-actions")
	}

	names, positional := positionals(target), target.Flags().Args()
	switch {
	case len(names) == 0:
	case len(positional) > 0:
		if names[0] == "slug" {
			slug = positional[0]
		}
	case names[0] == "slug":
		slug = in("slug")
		if slug == "" && group == "activity" {
			slug = gha.Slug(repo, env("GITHUB_RUN_ID"), env("GITHUB_JOB"))
		}
		if slug == "" {
			return nil, "", usagef("%s needs a slug: set the slug input or put it in command", target.CommandPath())
		}
		argv = append(argv, slug)
	case names[0] == "id":
		return nil, "", usagef("%s needs an id: put it in command, like %q", target.CommandPath(), strings.TrimPrefix(target.CommandPath(), "pushward ")+" 123")
	}
	return argv, slug, nil
}

// ghaOutputs derives the step outputs from the last response, so no command
// has to know about the Action. status is the resource's own status or
// state, or a notification's delivery; answer covers both a notification
// answer and an approval activity's.
func ghaOutputs(body []byte, status, slug string) [][2]string {
	m := decode(body)
	// Outputs share the job's environment size limit; a log activity with its
	// backlog can be larger than is sensible to pass between steps.
	const maxResponse = 512 << 10
	resp := bytes.TrimSpace(body)
	if len(resp) > maxResponse {
		resp = nil
	}
	return [][2]string{
		{"response", string(resp)},
		{"id", cmp.Or(str(m, "id"), str(m, "notification_id"))},
		{"slug", cmp.Or(str(m, "slug"), slug)},
		{"answer", cmp.Or(str(m, "action_id"), str(m, "content", "answer", "option"))},
		{"answer-text", str(m, "text")},
		{"status", cmp.Or(status, str(m, "status"), str(m, "state"), str(m, "delivery"))},
	}
}
