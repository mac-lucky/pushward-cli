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

PUSHWARD_API_TOKEN, when set, wins over the stored key.`,
	}
	c.AddCommand(newAuthLoginCmd(a), newAuthStatusCmd(a), newAuthLogoutCmd(a))
	return group(c)
}

func newAuthLoginCmd(a *App) *cobra.Command {
	var withToken bool
	c := &cobra.Command{
		Use:   "login",
		Short: "Check a key against the API and store it",
		Long: `Read a key, check it against the API, and store it in the config file
(mode 0600). With --with-token the key is read from stdin:

  op read op://Private/pushward/credential | pushward auth login --with-token`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := a.readToken(withToken)
			if err != nil {
				return err
			}
			if token == "" {
				return usagef("no key given")
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
	return c
}

func (a *App) readToken(fromStdin bool) (string, error) {
	if !fromStdin && a.StdinTTY {
		fmt.Fprint(a.Stderr, "Paste your integration key: ")
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
			resp, err := a.call(cmd.Context(), "getMe", nil, nil, nil)
			if err != nil {
				return err
			}
			cfg := a.cfg
			source := cfg.TokenSource
			if source == "env" {
				source = config.EnvToken
			}
			return a.out().Print(resp.Body, func(w io.Writer) error {
				fmt.Fprintf(w, "api          %s\n", cfg.APIURL)
				fmt.Fprintf(w, "key          %s (from %s)\n", maskToken(cfg.Token), source)
				return writeMe(w, decode(resp.Body))
			})
		},
	}
}

func maskToken(t string) string {
	if len(t) <= 10 {
		return strings.Repeat("*", len(t))
	}
	return t[:8] + "..." + t[len(t)-2:]
}

func newAuthLogoutCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored key",
		Args:  checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			f, path, err := config.Read()
			if err != nil {
				return err
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
}
