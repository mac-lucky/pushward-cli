package cmd

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/net/idna"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/output"
)

// The Enterprise plan's org commands: verified email domains, the single
// sign-on connection and the audit log export.

const (
	codeDomainNotVerified = "org.domain_not_verified"
	codePermissionDenied  = "org.permission_denied"
)

// verifyRetry is how often domain verify --wait looks the record up again.
var verifyRetry = 15 * time.Second

// exportTimeout bounds one audit export, reading the CSV included.
var exportTimeout = 10 * time.Minute

// timedOut is a --wait that ran out on err. It exits 7, as every --wait does.
type timedOut struct{ err error }

func (e *timedOut) Error() string   { return e.err.Error() + " (--wait ran out)" }
func (e *timedOut) Unwrap() []error { return []error{e.err, errWaitTimeout} }

func newOrgDomainCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "domain",
		Aliases: []string{"domains"},
		Short:   "Email domains the organization verifies, for single sign-on (Enterprise plan)",
		Long: `Claim an email domain, publish the TXT record add prints, then verify it.
Single sign-on admits addresses on verified domains only. A domain is
verified by one organization at most, and a verified domain lapses after
seven daily checks in a row miss the record.

  pushward org domain add example.com
  pushward org domain verify example.com --wait 30m
  pushward org domain list`,
	}
	c.AddCommand(
		&cobra.Command{
			Use:         "list",
			Short:       "List the domains and their status (--json shows the TXT records)",
			Args:        checkArgs,
			Annotations: operation("listOrgDomains"),
			RunE: func(cmd *cobra.Command, _ []string) error {
				resp, err := a.call(cmd.Context(), "listOrgDomains", nil, nil, nil)
				if err != nil {
					return err
				}
				return a.out().Print(resp.Body, func(w io.Writer) error {
					domains, err := decodeDomains(resp.Body)
					if err != nil {
						return err
					}
					rows := make([][]string, 0, len(domains))
					for _, d := range domains {
						rows = append(rows, []string{str(d, "id"), str(d, "domain"), str(d, "status"),
							shortTime(str(d, "verified_at")), shortTime(str(d, "last_checked_at"))})
					}
					return output.Table(w, []string{"ID", "DOMAIN", "STATUS", "VERIFIED", "LAST CHECKED"}, rows)
				})
			},
		},
		&cobra.Command{
			Use:   "add <domain>",
			Short: "Claim a domain and print the TXT record that verifies it",
			Long: `Claim an email domain. It stays pending until the TXT record this prints
is published in its DNS and pushward org domain verify finds it.

  pushward org domain add example.com`,
			Args:        checkArgs,
			Annotations: operation("createOrgDomain"),
			RunE: func(cmd *cobra.Command, argv []string) error {
				resp, err := a.call(cmd.Context(), "createOrgDomain", nil, nil, map[string]any{"domain": argv[0]})
				if err != nil {
					return err
				}
				m := decode(resp.Body)
				return a.out().Print(resp.Body, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "added %s (%s). Create this DNS record, then run pushward org domain verify %s\n  type   TXT\n  name   %s\n  value  %s\n",
						str(m, "domain"), str(m, "status"), str(m, "domain"), str(m, "txt_name"), str(m, "txt_value"))
					return err
				})
			},
		},
		newOrgDomainVerifyCmd(a),
		&cobra.Command{
			Use:         "remove <domain-or-id>",
			Short:       "Remove a domain (refused for the last verified one while single sign-on is on)",
			Args:        checkArgs,
			Annotations: operation("deleteOrgDomain"),
			RunE: func(cmd *cobra.Command, argv []string) error {
				id, err := a.domainID(cmd.Context(), argv[0])
				if err != nil {
					return err
				}
				if _, err := a.call(cmd.Context(), "deleteOrgDomain", map[string]string{"domain_id": id}, nil, nil); err != nil {
					return err
				}
				a.Status = "removed"
				a.out().Human("removed domain %s", argv[0])
				return nil
			},
		},
	)
	return group(c)
}

func newOrgDomainVerifyCmd(a *App) *cobra.Command {
	var wait time.Duration
	c := &cobra.Command{
		Use:   "verify <domain-or-id>",
		Short: "Look the domain's TXT record up and verify the domain",
		Long: `Have the server look the domain's TXT record up, once. With --wait it
tries again every 15s while the record is not visible yet, the lookup
gets no answer or the connection fails, until the duration runs out
(exit 7); any other refusal ends it at once.

  pushward org domain verify example.com --wait 30m`,
		Args:        checkArgs,
		Annotations: operation("verifyOrgDomain"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if wait < 0 {
				return usagef("--wait must not be negative")
			}
			ctx := cmd.Context()
			id, err := a.domainID(ctx, argv[0])
			if err != nil {
				return err
			}
			deadline := a.Now().Add(wait)
			for {
				resp, err := a.call(ctx, "verifyOrgDomain", map[string]string{"domain_id": id}, nil, nil)
				var ae *api.Error
				var te *api.TransportError
				notYet := errors.As(err, &ae) && ae.Code == codeDomainNotVerified
				// A 503 is a DNS lookup that got no answer (or the API busy), which
				// counts nothing. The client never retries a POST that got no
				// response, but verify is safe to repeat.
				noAnswer := ae != nil && ae.Status == http.StatusServiceUnavailable || errors.As(err, &te) && ctx.Err() == nil
				switch {
				case err == nil:
					m := decode(resp.Body)
					return a.out().Printf(resp.Body, "verified %s", str(m, "domain"))
				case wait == 0 || !notYet && !noAnswer:
					return err
				case a.Now().Add(verifyRetry).After(deadline):
					return &timedOut{err}
				}
				if !a.quiet {
					why := "the TXT record of " + argv[0] + " is not visible yet"
					switch {
					case te != nil:
						why = err.Error()
					case !notYet:
						why = "the lookup of " + argv[0] + " got no answer"
					}
					fmt.Fprintf(a.Stderr, "%s; trying again in %s\n", why, verifyRetry)
				}
				if err := a.sleep(ctx, verifyRetry); err != nil {
					return err
				}
			}
		},
	}
	c.Flags().DurationVar(&wait, "wait", 0, "keep trying every 15s while the TXT record is not visible yet or no answer comes back, up to this long (e.g. 30m)")
	return c
}

func decodeDomains(body []byte) ([]map[string]any, error) {
	var list struct {
		Domains []map[string]any `json:"domains"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decoding the domains: %w", err)
	}
	return list.Domains, nil
}

// domainID returns the id of the organization's domain arg names, by its id
// or by the domain itself.
func (a *App) domainID(ctx context.Context, arg string) (string, error) {
	if keyIDPattern.MatchString(arg) {
		return arg, nil
	}
	resp, err := a.call(ctx, "listOrgDomains", nil, nil, nil)
	if err != nil {
		return "", err
	}
	domains, err := decodeDomains(resp.Body)
	if err != nil {
		return "", err
	}
	want := strings.TrimSuffix(strings.ToLower(arg), ".")
	// The server keeps an internationalized domain in this form (punycode).
	if ascii, err := idna.Registration.ToASCII(want); err == nil {
		want = ascii
	}
	for _, d := range domains {
		if str(d, "domain") == want {
			return str(d, "id"), nil
		}
	}
	return "", fmt.Errorf("the organization has no domain %s (pushward org domain list shows them): %w", want, errNotFound)
}

func newOrgSSOCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "sso",
		Short: "The organization's single sign-on to the console (Enterprise plan)",
		Long: `Console sign-in through the organization's own identity provider, OpenID
Connect or SAML 2.0, for addresses on its verified domains (pushward org
domain). The org admin key can turn it on or off, change require_sso and
rotate the OIDC client secret. Setting it up and changing its identity
provider take an owner, in the PushWard console. show prints the redirect
URI, or the SAML entity ID and ACS URL, to register at the provider.

  pushward org sso show
  pushward org sso set --enabled --require-sso
  pushward org sso set --client-secret-stdin < secret.txt
  pushward org sso delete`,
	}
	c.AddCommand(
		a.getCmd("show", "Show the connection and the values to register at the identity provider", "getOrgSSOConnection"),
		newOrgSSOSetCmd(a),
		newOrgSSODeleteCmd(a),
	)
	return group(c)
}

func newOrgSSOSetCmd(a *App) *cobra.Command {
	var protocol, issuer, clientID, metadataFile, metadataURL string
	var secretStdin bool
	c := &cobra.Command{
		Use:   "set",
		Short: "Set up single sign-on or change it",
		Long: `Save the connection. Flags you leave out keep the stored value: --enabled
and --require-sso (--enabled=false turns it off), the protocol, and the
OIDC issuer and client id. SAML metadata left out keeps the stored identity
provider. The OIDC client secret is read from stdin, never from a flag, and
is never shown again.

With the org admin key this turns single sign-on on or off, changes
--require-sso and rotates the OIDC client secret. Setting it up and
changing its identity provider (the protocol, the OIDC issuer or client
id, the SAML metadata) take an owner, in the PushWard console: the key
gets 403 for those. Turning it on needs a verified domain. --require-sso
makes it the only console sign-in for members other than owners.

  pushward org sso set --enabled --require-sso
  pushward org sso set --enabled=false
  pushward org sso set --client-secret-stdin < secret.txt`,
		Args:        checkArgs,
		Annotations: operation("putOrgSSOConnection"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			oidcFlags := issuer != "" || clientID != "" || secretStdin
			samlFlags := metadataFile != "" || metadataURL != ""
			conflict := func(p string) error {
				switch {
				case p == "oidc" && samlFlags:
					return usagef("--metadata-file and --metadata-url are for --protocol saml")
				case p == "saml" && oidcFlags:
					return usagef("--issuer, --client-id and --client-secret-stdin are for --protocol oidc")
				}
				return nil
			}
			switch {
			case protocol != "" && protocol != "oidc" && protocol != "saml":
				return usagef("--protocol: want oidc or saml, got %q", protocol)
			case oidcFlags && samlFlags:
				return usagef("OIDC flags (--issuer, --client-id, --client-secret-stdin) and SAML flags (--metadata-file, --metadata-url) cannot be used together")
			case metadataFile != "" && metadataURL != "":
				return usagef("--metadata-file and --metadata-url cannot be used together")
			}
			if err := conflict(protocol); err != nil {
				return err
			}
			var metadata []byte
			if metadataFile != "" {
				var err error
				if metadata, err = os.ReadFile(metadataFile); err != nil { // #nosec G304 -- a file the user names
					return usagef("--metadata-file: %v", err)
				}
			}
			var secret string
			if secretStdin {
				var err error
				if secret, err = a.readSecret("Client secret: ", false); err != nil {
					return err
				}
				if secret == "" {
					return usagef("--client-secret-stdin: no client secret on stdin")
				}
			}

			ctx := cmd.Context()
			resp, err := a.call(ctx, "getOrgSSOConnection", nil, nil, nil)
			if err != nil {
				return err
			}
			cur := decode(resp.Body)
			stored := str(cur, "protocol")
			if protocol == "" {
				if protocol = stored; protocol == "" {
					return usagef("there is no single sign-on yet: set it up with --protocol oidc or --protocol saml")
				}
				if err := conflict(protocol); err != nil {
					return err
				}
			}
			keep := func(flag, key string) bool {
				if cmd.Flags().Changed(flag) {
					v, _ := cmd.Flags().GetBool(flag)
					return v
				}
				v, _ := cur[key].(bool)
				return v
			}
			b := map[string]any{"protocol": protocol, "enabled": keep("enabled", "enabled"), "require_sso": keep("require-sso", "require_sso")}
			// Only an owner names the identity provider: a new connection, another
			// protocol, OIDC issuer or client id, or SAML metadata.
			newProvider := protocol != stored || metadata != nil || metadataURL != ""
			if protocol == "oidc" {
				var was map[string]any
				if stored == "oidc" {
					was, _ = cur["oidc"].(map[string]any)
				}
				o := map[string]any{"issuer": cmp.Or(issuer, str(was, "issuer")), "client_id": cmp.Or(clientID, str(was, "client_id"))}
				if o["issuer"] == "" || o["client_id"] == "" {
					return usagef("--issuer and --client-id are needed to set up OIDC")
				}
				newProvider = newProvider || o["issuer"] != str(was, "issuer") || o["client_id"] != str(was, "client_id")
				if secretStdin {
					o["client_secret"] = secret
				}
				b["oidc"] = o
			} else {
				s := map[string]any{}
				if metadata != nil {
					s["idp_metadata_xml"] = string(metadata)
				}
				if metadataURL != "" {
					s["idp_metadata_url"] = metadataURL
				}
				b["saml"] = s
			}
			resp, err = a.call(ctx, "putOrgSSOConnection", nil, nil, b)
			if err != nil {
				if newProvider {
					err = ownerHint(err)
				}
				return err
			}
			m := decode(resp.Body)
			return a.out().Printf(resp.Body, "saved %s single sign-on: enabled %s, require_sso %s",
				str(m, "protocol"), str(m, "enabled"), str(m, "require_sso"))
		},
	}
	c.Flags().StringVar(&protocol, "protocol", "", "oidc or saml (default: the stored one)")
	c.Flags().StringVar(&issuer, "issuer", "", "OIDC: the identity provider's https issuer URL")
	c.Flags().StringVar(&clientID, "client-id", "", "OIDC: the client id registered at the identity provider")
	c.Flags().BoolVar(&secretStdin, "client-secret-stdin", false, "OIDC: read the client secret from stdin (prompted for on a terminal)")
	c.Flags().StringVar(&metadataFile, "metadata-file", "", "SAML: the identity provider's metadata XML file")
	c.Flags().StringVar(&metadataURL, "metadata-url", "", "SAML: an https URL the server reads the metadata from once")
	c.Flags().Bool("enabled", false, "whether people sign in with it (needs a verified domain)")
	c.Flags().Bool("require-sso", false, "make it the only console sign-in for members other than owners")
	return c
}

// ownerHint adds where to go when the server keeps a change to owners.
func ownerHint(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden && ae.Code == codePermissionDenied {
		return fmt.Errorf("%w\nhint: an owner can make this change in the PushWard console", err)
	}
	return err
}

func newOrgSSODeleteCmd(a *App) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "delete",
		Short: "Remove single sign-on (console sessions it signed in stay valid until they expire)",
		Long: `Remove the connection, after a prompt unless --yes. Members then sign in
to the console the other ways.`,
		Args:        checkArgs,
		Annotations: operation("deleteOrgSSOConnection"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprint(a.Stderr, "Remove the organization's single sign-on? [y/N] ")
				line, err := bufio.NewReader(a.Stdin).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
					return errors.New("canceled: single sign-on was not removed")
				}
			}
			if _, err := a.call(cmd.Context(), "deleteOrgSSOConnection", nil, nil, nil); err != nil {
				return err
			}
			a.Status = "deleted"
			a.out().Human("removed single sign-on")
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "remove it without asking")
	return c
}

func newOrgAuditExportCmd(a *App) *cobra.Command {
	var from, to, out string
	c := &cobra.Command{
		Use:   "export",
		Short: "Save the audit log as CSV (Enterprise plan)",
		Long: `Save the events created from --from (inclusive) to --to (exclusive) as
CSV, oldest first. Each takes a date (midnight UTC) or an RFC 3339 time;
--to defaults to now and --from to 365 days before --to. One export covers
at most 366 days. The file goes to the current directory under the name
the server gives it, never over an existing file, unless -o names one;
-o - writes the CSV to stdout.

  pushward org audit export --from 2026-01-01 --to 2026-04-01
  pushward org audit export --from 2026-10-01T00:00:00Z -o -`,
		Args:        checkArgs,
		Annotations: operation("exportOrgAuditEvents"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for _, f := range []struct{ name, val string }{{"from", from}, {"to", to}} {
				if f.val == "" {
					continue
				}
				t, err := parseDay(f.val)
				if err != nil {
					return usagef("--%s: %v", f.name, err)
				}
				q.Set(f.name, t.UTC().Format(time.RFC3339))
			}
			// Ctrl-C cancels the download, and the temporary file, which holds
			// members' addresses, is removed on the way out.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, params, err := a.orgClient(ctx, nil)
			if err != nil {
				return err
			}
			op := api.Op("exportOrgAuditEvents")
			path, err := op.Expand(params)
			if err != nil {
				return err
			}
			// The file is opened before the request: the server records every
			// export and allows only a few a minute.
			var tmp *os.File
			if out != "-" {
				if info, err := os.Stat(out); err == nil && info.IsDir() {
					return usagef("-o: %s is a directory; name the file to save to", out)
				}
				if tmp, err = os.CreateTemp(filepath.Dir(out), ".pushward-audit-*"); err != nil {
					return fmt.Errorf("saving the export: %w", err)
				}
				defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }() // a rename took it away already, a link did not
			}
			// Every try that reaches the server counts as an export, so a 5xx or a
			// dropped connection is not tried again.
			resp, err := c.Do(ctx, api.Request{Method: op.Method, Path: path, Query: q, Stream: true, Timeout: exportTimeout,
				Header: http.Header{"Accept": {"text/csv, application/json"}}, NoReplay: true})
			if err != nil {
				return err
			}
			defer resp.Stream.Close()
			if out == "-" {
				_, err := io.Copy(a.Stdout, resp.Stream)
				return err
			}
			// A name the server picked never replaces a file, as with curl -J.
			name := cmp.Or(out, attachmentName(resp.Header.Get("Content-Disposition")), "pushward-audit.csv")
			n, err := saveFile(tmp, name, resp.Stream, out != "")
			if err != nil {
				return fmt.Errorf("saving the export: %w", err)
			}
			a.Status = "saved"
			if a.Body, err = json.Marshal(map[string]any{"path": name, "bytes": n}); err != nil {
				return err
			}
			return a.out().Printf(a.Body, "saved %s (%d bytes)", name, n)
		},
	}
	c.Flags().StringVar(&from, "from", "", "first day or time to export: 2026-01-01 or RFC 3339 (default: 365 days before --to)")
	c.Flags().StringVar(&to, "to", "", "day or time to stop before: 2026-04-01 or RFC 3339 (default: now)")
	c.Flags().StringVarP(&out, "output", "o", "", "file to save the CSV to, - for stdout (default: the server's file name)")
	return c
}

// parseDay reads a date, as midnight UTC, or an RFC 3339 time.
func parseDay(raw string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("want a date (2026-10-01) or RFC 3339 (2026-10-01T09:00:00Z), got %q", raw)
}

// attachmentName is the file name a Content-Disposition header gives, or ""
// when it gives none that stays in the current directory and shows in a
// listing (a dot file such as .bashrc or .git does not).
func attachmentName(header string) string {
	_, params, err := mime.ParseMediaType(header)
	name := params["filename"]
	if err != nil || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
}

// saveFile writes r to tmp, a temporary file next to path, and then moves it
// to path, so a download that fails part way leaves no partial file behind.
// Unless replace, a path that exists by then is refused: a hard link fails
// on one, however late it appeared, where a rename would replace it. It
// closes tmp; the caller removes tmp's name.
func saveFile(tmp *os.File, path string, r io.Reader, replace bool) (int64, error) {
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case replace:
		err = os.Rename(tmp.Name(), path)
	default:
		if err = os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s already exists; name the file with -o", path)
		} else if err != nil {
			// A file system without hard links (FAT, some network shares) ends up here.
			err = fmt.Errorf("%w; name the file with -o", err)
		}
	}
	return n, err
}
