package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func NewRoot(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "pushward",
		Short: "Send PushWard notifications and drive Live Activities and widgets",
		Long: `pushward talks to the PushWard API with an integration key (hlk_...).

Set PUSHWARD_API_TOKEN, or run "pushward auth login" once to store the key.
On a terminal, commands print a short summary; piped or with --json they
print the API response as JSON.`,
		Version:       a.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Compile --jq before any request, so a typo fails before a
		// notification goes out.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.out().Compile()
		},
	}
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.SetVersionTemplate(a.versionLine() + "\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &UsageError{err} })

	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", false, "print the raw JSON response, even on a terminal")
	pf.StringVar(&a.jq, "jq", "", "filter the JSON response with a jq expression")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "print nothing on success")
	pf.BoolVar(&a.debug, "debug", false, "log each HTTP request to stderr (never the key or bodies)")
	pf.StringVar(&a.apiURL, "api-url", "", "API base URL (default https://api.pushward.app, or $PUSHWARD_API_URL)")

	root.AddCommand(
		newActivityCmd(a),
		newNotifyCmd(a),
		newNotificationCmd(a),
		newScheduleCmd(a),
		newReceiptCmd(a),
		newE2ECmd(a),
		newWidgetCmd(a),
		newKeyCmd(a),
		newEmailCmd(a),
		newMeCmd(a),
		newHealthCmd(a),
		newAPICmd(a),
		newAuthCmd(a),
		newVersionCmd(a),
		newGHACmd(a),
	)
	return group(root)
}

// group lets a command that only holds subcommands reject an unknown one as
// a usage error. Cobra would print help and exit 0 for a subgroup, and exit
// 1 with a bare error for the root.
func group(c *cobra.Command) *cobra.Command {
	c.Args = cobra.ArbitraryArgs
	c.SuggestionsMinimumDistance = 2 // cobra's own default, applied only on its error path
	c.RunE = func(cmd *cobra.Command, argv []string) error {
		if len(argv) == 0 {
			return cmd.Help()
		}
		msg := fmt.Sprintf("unknown command %q for %q", argv[0], cmd.CommandPath())
		if s := cmd.SuggestionsFor(argv[0]); len(s) > 0 {
			msg += fmt.Sprintf("; did you mean %q?", s[0])
		}
		return usagef("%s", msg)
	}
	return c
}

func (a *App) versionLine() string {
	return fmt.Sprintf("pushward %s (%s, %s) %s/%s", a.Version, a.Commit, a.Date, runtime.GOOS, runtime.GOARCH)
}

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), a.versionLine())
			return err
		},
	}
}
