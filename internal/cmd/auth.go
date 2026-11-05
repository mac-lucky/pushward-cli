package cmd

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mac-lucky/pushward-cli/internal/config"
)

func newAuthCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "auth",
		Short: "Store, check or remove your integration key",
		Long: `Integration keys (hlk_...) are created in the PushWard app's settings.
A key can be limited to some activity slugs and to notifications, widgets
or emails. The account's default key can also create and revoke other keys
with pushward key.

An organization's admin key (hlo_...) is stored next to it with
pushward auth login --org and used only by the pushward org commands.

PUSHWARD_API_TOKEN, when set, wins over the stored key, and
PUSHWARD_ORG_API_TOKEN over the stored org admin key.`,
	}
	c.AddCommand(newAuthLoginCmd(a), newAuthStatusCmd(a), newAuthLogoutCmd(a))
	return group(c)
}

func newAuthLoginCmd(a *App) *cobra.Command {
	var withToken, org bool
	c := &cobra.Command{
		Use:   "login",
		Short: "Check a key against the API and store it",
		Long: `Read a key, check it against the API, and store it in the config file
(mode 0600). With --with-token the key is read from stdin:

  op read op://Private/pushward/credential | pushward auth login --with-token

With --org it stores an org admin key (hlo_...) from the PushWard console
instead, for the pushward org commands; the integration key stays as it is.`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org {
				return a.loginOrg(cmd, withToken)
			}
			token, err := a.readSecret("Paste your integration key: ", withToken)
			if err != nil {
				return err
			}
			if token == "" {
				return usagef("no key given")
			}
			if strings.HasPrefix(token, "hlo_") {
				return usagef("hlo_ keys administer an organization: store it with pushward auth login --org")
			}
			if !strings.HasPrefix(token, "hlk_") {
				a.out().Warnf("integration keys start with hlk_; this one does not")
			}
			if _, err := a.api(); err != nil {
				return usagef("%v", err)
			}
			resp, err := a.newClient(a.cfg.APIURL, token).Call(cmd.Context(), "getMe", nil, nil, nil)
			if err != nil {
				return fmt.Errorf("checking the key: %w", err)
			}
			me := decode(resp.Body)
			f, _, err := config.Read()
			if err != nil {
				return err
			}
			f.Token = token
			if a.apiURL != "" {
				f.APIURL = a.cfg.APIURL
			}
			path, err := config.Write(f)
			if err != nil {
				return err
			}
			a.out().Human("logged in as %s; key stored in %s", cmp.Or(str(me, "nickname"), str(me, "id")), path)
			if os.Getenv(config.EnvToken) != "" {
				a.out().Warnf("%s is set and takes precedence over the stored key", config.EnvToken)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&withToken, "with-token", false, "read the key from stdin")
	c.Flags().BoolVar(&org, "org", false, "store an org admin key (hlo_...) for the pushward org commands")
	return c
}

// loginOrg stores an org admin key after GET /orgs accepts it; /auth/me
// refuses these keys.
func (a *App) loginOrg(cmd *cobra.Command, withToken bool) error {
	token, err := a.readSecret("Paste your org admin key: ", withToken)
	if err != nil {
		return err
	}
	if token == "" {
		return usagef("no key given")
	}
	if !strings.HasPrefix(token, "hlo_") {
		return usagef("org admin keys start with hlo_; store an integration key (hlk_...) with pushward auth login, without --org")
	}
	if _, err := a.api(); err != nil {
		return usagef("%v", err)
	}
	org, err := keyOrg(cmd.Context(), a.newClient(a.cfg.APIURL, token))
	if err != nil {
		return fmt.Errorf("checking the key: %w", err)
	}
	f, _, err := config.Read()
	if err != nil {
		return err
	}
	f.OrgToken = token
	if a.apiURL != "" {
		f.APIURL = a.cfg.APIURL
	}
	path, err := config.Write(f)
	if err != nil {
		return err
	}
	a.out().Human("logged in to %s (%s) as %s; org admin key stored in %s", str(org, "name"), str(org, "id"), str(org, "role"), path)
	if os.Getenv(config.EnvOrgToken) != "" {
		a.out().Warnf("%s is set and takes precedence over the stored org admin key", config.EnvOrgToken)
	}
	return nil
}

// readSecret prompts for a secret without echoing it, or reads one line from
// stdin when fromStdin is set or stdin is not a terminal.
func (a *App) readSecret(prompt string, fromStdin bool) (string, error) {
	if !fromStdin && a.StdinTTY {
		fmt.Fprint(a.Stderr, prompt)
		f, ok := a.Stdin.(*os.File)
		if !ok {
			return "", errors.New("stdin is not a terminal")
		}
		b, err := term.ReadPassword(int(f.Fd())) // #nosec G115 -- a file descriptor fits in int
		fmt.Fprintln(a.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func newAuthStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which key is in use and the account behind it",
		Args:  checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.api(); err != nil {
				return err
			}
			cfg := a.cfg
			// The org admin key slot is shown, not checked: /auth/me refuses it.
			org := fmt.Sprintf("org key      %s\n", slot(cfg.OrgToken, cfg.OrgTokenSource, config.EnvOrgToken))
			if cfg.Token == "" && cfg.OrgToken != "" {
				a.out().Human("api          %s\nkey          none\n%s", cfg.APIURL, strings.TrimSuffix(org, "\n"))
			}
			resp, err := a.call(cmd.Context(), "getMe", nil, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, func(w io.Writer) error {
				fmt.Fprintf(w, "api          %s\n", cfg.APIURL)
				fmt.Fprintf(w, "key          %s\n", slot(cfg.Token, cfg.TokenSource, config.EnvToken))
				fmt.Fprint(w, org)
				return writeMe(w, decode(resp.Body))
			})
		},
	}
}

// slot renders a stored key masked, with where it came from.
func slot(token, source, env string) string {
	if token == "" {
		return "none"
	}
	if source == "env" {
		source = env
	}
	return fmt.Sprintf("%s (from %s)", maskToken(token), source)
}

func maskToken(t string) string {
	if len(t) <= 10 {
		return strings.Repeat("*", len(t))
	}
	return t[:8] + "..." + t[len(t)-2:]
}

func newAuthLogoutCmd(a *App) *cobra.Command {
	var org bool
	c := &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored key",
		Args:  checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			f, path, err := config.Read()
			if err != nil {
				return err
			}
			if org {
				if f.OrgToken == "" {
					a.out().Human("no org admin key stored in %s", path)
					return nil
				}
				f.OrgToken = ""
				if _, err := config.Write(f); err != nil {
					return err
				}
				a.out().Human("removed the org admin key from %s", path)
				if os.Getenv(config.EnvOrgToken) != "" {
					a.out().Warnf("%s is still set in this shell", config.EnvOrgToken)
				}
				return nil
			}
			if f.Token == "" {
				a.out().Human("no key stored in %s", path)
				return nil
			}
			f.Token = ""
			if _, err := config.Write(f); err != nil {
				return err
			}
			a.out().Human("removed the key from %s", path)
			if os.Getenv(config.EnvToken) != "" {
				a.out().Warnf("%s is still set in this shell", config.EnvToken)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&org, "org", false, "remove only the stored org admin key")
	return c
}
