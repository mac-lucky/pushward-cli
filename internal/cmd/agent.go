package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mac-lucky/pushward-cli/internal/agent"
	"github.com/mac-lucky/pushward-cli/internal/config"
)

// hookAnnotation marks the commands a coding agent runs as its hooks. They
// exit 0 whatever happens: exit 2 from a Claude Code PreToolUse hook blocks
// the tool, and any other failure must leave the prompt to the terminal.
const hookAnnotation = "agent-hook"

var agents = []agent.Agent{agent.Claude, agent.Codex}

func newAgentCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "agent",
		Short: "Answer Claude Code and Codex prompts from your phone",
		Long: `Claude Code and Codex run pushward as a hook. A permission prompt, or a
question Claude asks, goes to your phone as an approval Live Activity, and
your answer goes back to the agent. When no device shows the card within
15s, the same question comes as a notification with buttons.

On a Mac you used in the last 90s, the prompt stays in the terminal; touch
the Mac while one waits on the phone and it comes back to the terminal.
Linux has no such check: every prompt goes to the phone. Needs-input and
finished notices go out only while you are away.

  pushward agent install claude
  pushward agent install codex
  pushward agent status
  pushward agent test

The command, file or question shown on the card is sent to PushWard as the
card's text. The hooks need a personal key: anyone the card reaches can
answer it, so with an organization's key they send nothing.`,
	}
	c.AddCommand(
		newAgentHookCmd(a),
		newAgentInstallCmd(a, false),
		newAgentInstallCmd(a, true),
		newAgentStatusCmd(a),
		newAgentTestCmd(a),
	)
	return group(c)
}

// agentFlags are the options of agent hook and agent test.
type agentFlags struct {
	opts agent.Options
	log  string
}

func (f *agentFlags) register(c *cobra.Command, hook bool) {
	d := agent.DefaultOptions()
	f.opts = d
	fl := c.Flags()
	maxWait := d.MaxWait
	if !hook {
		maxWait = 10 * time.Minute
	}
	fl.DurationVar(&f.opts.MaxWait, "max-wait", maxWait, "how long to wait for an answer from the phone")
	fl.StringVar(&f.opts.Via, "via", d.Via, "auto (a card, a notification when no device shows it), card or notification")
	if !hook {
		return
	}
	fl.StringVar(&f.opts.OnTimeout, "on-timeout", d.OnTimeout, "when --max-wait passes: none (back to the terminal) or deny")
	fl.DurationVar(&f.opts.DeskIdle, "desk-idle", d.DeskIdle, "macOS: no input for this long means you are away from the desk")
	fl.StringVar(&f.opts.Desk, "desk", d.Desk, "auto (read the Mac's idle time), away or desk")
	fl.StringVar(&f.opts.Notices, "notices", d.Notices, "needs-input and finished notices: away, always or off")
	fl.StringVar(&f.log, "log", "", "append the log to this file instead of stderr")
}

// applyAgentEnv sets each hook flag the command line left out from
// PUSHWARD_AGENT_<FLAG>, so the hook command in the settings file stays the
// same while its options change.
func applyAgentEnv(cmd *cobra.Command, getenv func(string) string) error {
	var errs []error
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		name := "PUSHWARD_AGENT_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v := getenv(name); v != "" && !f.Changed {
			if err := cmd.Flags().Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	})
	return errors.Join(errs...)
}

func newAgentHookCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:         "hook",
		Short:       "Run as a Claude Code or Codex hook (agent install sets it up)",
		Annotations: map[string]string{hookAnnotation: "true"},
	}
	for _, ag := range agents {
		var f agentFlags
		sub := &cobra.Command{
			Use:   string(ag),
			Short: "Answer a " + ag.Name() + " hook from the phone",
			Long: `Read one hook call as JSON on stdin and print the decision on stdout, or
nothing to leave the prompt to the terminal. It always exits 0. Every flag
can also be set as PUSHWARD_AGENT_<FLAG>, for example
PUSHWARD_AGENT_ON_TIMEOUT=deny.

The log (stderr, or --log) holds slugs, ids and statuses, never a command,
typed text or a key.`,
			Args:        checkArgs,
			Annotations: map[string]string{hookAnnotation: "true"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				a.runHook(cmd, ag, &f)
				return nil
			},
		}
		f.register(sub, true)
		c.AddCommand(sub)
	}
	return group(c)
}

func (a *App) runHook(cmd *cobra.Command, ag agent.Agent, f *agentFlags) {
	logw := a.Stderr
	if err := applyAgentEnv(cmd, a.Getenv); err != nil {
		fmt.Fprintf(logw, "pushward agent: %v\n", err)
		return
	}
	if f.log != "" {
		lf, err := os.OpenFile(f.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- a log path the user chose
		if err == nil {
			defer lf.Close()
			logw = lf
		}
	}
	logger := log.New(logw, "pushward agent: ", log.LstdFlags)
	defer func() {
		// A panic exits 2, which Claude Code reads as "block the tool".
		if p := recover(); p != nil {
			logger.Printf("internal error")
		}
	}()
	if err := f.opts.Validate(); err != nil {
		logger.Printf("%v", err)
		return
	}
	// Closing the terminal tab sends SIGHUP: end the card and retract the
	// notification on the way out, and exit 0 like any other way out.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if out := a.agentRunner(f.opts, logger).Hook(ctx, ag, a.Stdin); len(out) > 0 {
		_, _ = a.Stdout.Write(out)
	}
}

// agentRunner backs the agent with the CLI's client. It calls the client
// itself rather than through call, which records each response for the
// Action: the card and the notification are read at the same time.
func (a *App) agentRunner(opts agent.Options, logger *log.Logger) *agent.Runner {
	c, keyErr := a.keyed()
	if keyErr != nil {
		logger.Printf("%v", keyErr)
	}
	send := func(ctx context.Context, op string, params map[string]string, query url.Values, body map[string]any) ([]byte, error) {
		var data []byte
		if body != nil {
			var err error
			if data, err = json.Marshal(body); err != nil {
				return nil, err
			}
		}
		resp, err := c.Call(ctx, op, params, query, data)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	call := func(ctx context.Context, op string, params map[string]string, query url.Values, body map[string]any) ([]byte, error) {
		if keyErr != nil {
			return nil, agent.ErrNoKey
		}
		if op != "createNotification" {
			return send(ctx, op, params, query, body)
		}
		// Sealed with the encryption key when one is set, as notify does. A
		// sealed send the server refuses is not sent again readable.
		sealed := maps.Clone(body)
		if _, err := a.encryptBody(sealed, false); err != nil {
			return nil, err
		}
		return send(ctx, op, params, query, sealed)
	}
	r := &agent.Runner{Call: call, Idle: agent.SystemIdle(), Log: logger, Opts: opts}
	if keyErr == nil {
		if dir, err := config.Dir(); err == nil {
			r.KeyCache = agent.NewKeyCache(filepath.Join(dir, agentKeyCache), c.BaseURL, c.Token)
			r.KeyCache.Now = a.Now
		}
	}
	return r
}

// agentKeyCache is the file in the config directory where the hooks remember
// a key that passed their check.
const agentKeyCache = "agent-key-check.json"

func newAgentInstallCmd(a *App, remove bool) *cobra.Command {
	prefix := "Add the PushWard hooks to"
	c := &cobra.Command{
		Use:   "install",
		Short: prefix + " Claude Code or Codex",
		Long: `Add the hooks to ~/.claude/settings.json (Claude Code) or
~/.codex/hooks.json (Codex), keeping every other setting and hook as it is.
The file as it was is copied to <file>.bak first, unless there is a .bak
already: that one is kept. Running it again changes nothing. Codex runs a
new hook only after you trust it once with /hooks.`,
	}
	if remove {
		prefix = "Take the PushWard hooks out of"
		c.Use = "uninstall"
		c.Short = prefix + " Claude Code or Codex"
		c.Long = `Remove the hooks agent install added, and nothing else. The file as it
was is copied to <file>.bak first, unless there is a .bak already.`
	}
	for _, ag := range agents {
		var dryRun bool
		var settings string
		var maxWait time.Duration
		sub := &cobra.Command{
			Use:   string(ag),
			Short: prefix + " " + ag.Name(),
			Args:  checkArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				path := settings
				if path == "" {
					var err error
					if path, err = agent.DefaultSettings(ag); err != nil {
						return err
					}
				}
				var plan agent.Plan
				var err error
				if remove {
					plan, err = agent.PlanUninstall(path, ag)
				} else {
					if maxWait < time.Second {
						return usagef("--max-wait must be at least 1s")
					}
					var command string
					if command, err = agent.HookCommand(ag, maxWait); err != nil {
						return err
					}
					plan, err = agent.PlanInstall(path, ag, command, maxWait)
				}
				if err != nil {
					return err
				}
				if dryRun {
					_, err := a.Stdout.Write(plan.After)
					return err
				}
				backup, wrote, err := plan.Apply()
				if err != nil {
					return err
				}
				a.agentInstalled(ag, plan, backup, wrote, remove)
				return nil
			},
		}
		sub.Flags().BoolVar(&dryRun, "dry-run", false, "print the resulting file instead of writing it")
		sub.Flags().StringVar(&settings, "settings", "", "the settings file (default: the agent's own)")
		if !remove {
			sub.Flags().DurationVar(&maxWait, "max-wait", agent.DefaultMaxWait, "how long a prompt waits for the phone; the hook's timeout is a minute more")
		}
		c.AddCommand(sub)
	}
	return group(c)
}

func (a *App) agentInstalled(ag agent.Agent, plan agent.Plan, backup string, wrote, removed bool) {
	if a.quiet {
		return
	}
	w := a.Stdout
	switch {
	case !plan.Changed() && removed:
		fmt.Fprintf(w, "%s has no PushWard hooks for %s\n", plan.Path, ag.Name())
		return
	case !plan.Changed():
		fmt.Fprintf(w, "%s already has the PushWard hooks for %s\n", plan.Path, ag.Name())
	case removed:
		fmt.Fprintf(w, "removed the PushWard hooks from %s\n", plan.Path)
	default:
		fmt.Fprintf(w, "added the PushWard hooks to %s\n", plan.Path)
	}
	switch {
	case wrote:
		fmt.Fprintf(w, "the file as it was is in %s\n", backup)
	case backup != "":
		fmt.Fprintf(w, "%s from before is kept as it was\n", backup)
	}
	if ag == agent.Codex && !removed {
		fmt.Fprintln(w, "in Codex, run /hooks once and trust the PushWard hooks: Codex skips a hook until it is trusted")
	}
}

func newAgentStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check the key, the desk check and which hooks are installed",
		Long: `Print whether a key is set and works (an organization's key does not
work for the hooks), whether you count as at the desk right now, going by
the PUSHWARD_AGENT_DESK and PUSHWARD_AGENT_DESK_IDLE the hook reads too,
and which agents have the hooks. Exits 3 when the key is missing or
refused.`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := a.Stdout
			var keyErr error
			if _, keyErr = a.keyed(); keyErr != nil {
				fmt.Fprintf(w, "key      none (%v)\n", keyErr)
			} else {
				src := a.cfg.TokenSource
				if src == "env" {
					src = "$PUSHWARD_API_TOKEN"
				}
				_, keyErr = a.call(cmd.Context(), "getMe", nil, nil, nil)
				var kindErr error
				if keyErr == nil {
					// Asked every time: status is how the user checks a key.
					r := a.agentRunner(agent.DefaultOptions(), log.New(io.Discard, "", 0))
					r.KeyCache = nil
					kindErr = r.CheckKey(cmd.Context())
				}
				switch {
				case keyErr != nil:
					fmt.Fprintf(w, "key      from %s, refused: %v\n", src, keyErr)
				case errors.Is(kindErr, agent.ErrOrgKey):
					fmt.Fprintf(w, "key      from %s, an organization's key: the hooks need a personal one and send nothing with it\n", src)
				case kindErr != nil:
					fmt.Fprintf(w, "key      from %s, %v\n", src, kindErr)
				default:
					fmt.Fprintf(w, "key      from %s, works\n", src)
				}
			}
			opts, err := a.hookOptions()
			fmt.Fprintf(w, "desk     %s\n", deskState(cmd.Context(), opts, err))
			for _, ag := range agents {
				path, err := agent.DefaultSettings(ag)
				if err != nil {
					fmt.Fprintf(w, "%-8s %v\n", ag, err)
					continue
				}
				events, err := agent.Installed(path, ag)
				switch {
				case err != nil:
					fmt.Fprintf(w, "%-8s %v\n", ag, err)
				case len(events) == 0:
					fmt.Fprintf(w, "%-8s no hooks in %s\n", ag, path)
				default:
					fmt.Fprintf(w, "%-8s %s in %s\n", ag, strings.Join(events, ", "), path)
				}
			}
			return keyErr
		},
	}
}

// hookOptions are the options a hook gets from the environment: the
// defaults, changed by PUSHWARD_AGENT_<FLAG> the way the hook reads them.
func (a *App) hookOptions() (agent.Options, error) {
	var f agentFlags
	c := &cobra.Command{Use: "hook"}
	f.register(c, true)
	if err := applyAgentEnv(c, a.Getenv); err != nil {
		return f.opts, err
	}
	return f.opts, f.opts.Validate()
}

func deskState(ctx context.Context, opts agent.Options, optsErr error) string {
	switch {
	case optsErr != nil:
		return fmt.Sprintf("%v: the hook leaves every prompt in the terminal", optsErr)
	case opts.Desk == "desk":
		return "at the desk (PUSHWARD_AGENT_DESK=desk): prompts stay in the terminal"
	case opts.Desk == "away":
		return "away (PUSHWARD_AGENT_DESK=away): prompts go to the phone"
	}
	idle := agent.SystemIdle()
	if idle == nil {
		return "no idle time to read here: every prompt goes to the phone"
	}
	d, err := idle(ctx)
	switch {
	case err != nil:
		return "could not read the idle time: prompts go to the phone"
	case d < opts.DeskIdle:
		return fmt.Sprintf("at the desk (idle %s): prompts stay in the terminal", d.Round(time.Second))
	}
	return fmt.Sprintf("away (idle %s): prompts go to the phone", d.Round(time.Second))
}

func newAgentTestCmd(a *App) *cobra.Command {
	var f agentFlags
	c := &cobra.Command{
		Use:   "test",
		Short: "Ask three sample questions on one card and print the answers",
		Long: `Ask three questions the way a Claude Code question is asked: one card
that steps through them, or notifications with --via notification, the
fallback. Prints each answer; exits 7 if nobody answers within --max-wait.
The desk check is skipped.

  pushward agent test
  pushward agent test --via notification`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.opts.Desk, f.opts.Notices = "away", "off"
			if err := f.opts.Validate(); err != nil {
				return usagef("%v", err)
			}
			if _, err := a.keyed(); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			var logw io.Writer = a.Stderr
			if a.quiet {
				logw = io.Discard
			}
			cwd, _ := os.Getwd()
			answers, err := a.agentRunner(f.opts, log.New(logw, "", 0)).Test(ctx, cwd)
			switch {
			case errors.Is(err, agent.ErrNoAnswer):
				return errWaitTimeout
			case err != nil:
				return err
			}
			body, err := json.Marshal(answers)
			if err != nil {
				return err
			}
			return a.out().Print(body, func(w io.Writer) error {
				for _, an := range answers {
					if _, err := fmt.Fprintf(w, "%s %s\n", an.Question, an.Answer); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f.register(c, false)
	return c
}
