package cmd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/body"
)

func newEmailCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "email",
		Short: "Send email to your verified recipients",
	}
	var bf bodyFlags
	var to, subject, text, textFile, html, htmlFile string
	send := &cobra.Command{
		Use:   "send",
		Short: "Send an email",
		Long: `Send an email. The recipient must be verified on your account and not
unsubscribed. Give a text body, an HTML body, or both; - reads stdin.

  pushward email send --to ops@example.com --subject "Nightly report" --text-file report.txt`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "sendEmail"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			conv := map[string]any{}
			if to != "" {
				conv["to"] = to
			}
			if subject != "" {
				conv["subject"] = subject
			}
			for _, f := range []struct{ key, name, inline, file string }{
				{"text_body", "text", text, textFile},
				{"html_body", "html", html, htmlFile},
			} {
				v, err := a.bodyText(f.name, f.inline, f.file)
				if err != nil {
					return err
				}
				if v != "" {
					conv[f.key] = v
				}
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "sendEmail", nil, nil, b)
			if err != nil {
				return err
			}
			e := decode(resp.Body)
			if d := str(e, "delivery"); d == "none" {
				a.out().Warnf("email not delivered (%s)", cmp.Or(str(e, "reason"), "no reason given"))
			}
			return a.out().Printf(resp.Body, "email %s to %s: %s", str(e, "id"), str(e, "to"), str(e, "status"))
		},
	}
	send.Flags().StringVar(&to, "to", "", "recipient address (required)")
	send.Flags().StringVar(&subject, "subject", "", "subject line (required)")
	send.Flags().StringVar(&text, "text", "", "plain-text body, or - for stdin")
	send.Flags().StringVar(&textFile, "text-file", "", "read the plain-text body from a file")
	send.Flags().StringVar(&html, "html", "", "HTML body, or - for stdin")
	send.Flags().StringVar(&htmlFile, "html-file", "", "read the HTML body from a file")
	bf.register(send)
	c.AddCommand(send)
	return group(c)
}

// bodyText resolves one of email's body flags: inline text, - for stdin,
// or its -file twin.
func (a *App) bodyText(name, inline, file string) (string, error) {
	arg := inline
	switch {
	case inline != "" && file != "":
		return "", usagef("--%s and --%s-file are mutually exclusive", name, name)
	case file != "":
		arg = "@" + file
	case inline != "-":
		return inline, nil
	}
	b, _, err := body.Read(arg, a.Stdin)
	if err != nil {
		return "", usagef("--%s: %v", name, err)
	}
	return string(b), nil
}

func newMeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "me",
		Short:       "Show the account behind the key and its quotas",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getMe"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := a.call(cmd.Context(), "getMe", nil, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, func(w io.Writer) error {
				return writeMe(w, decode(resp.Body))
			})
		},
	}
}

func writeMe(w io.Writer, u map[string]any) error {
	name := cmp.Or(str(u, "nickname"), str(u, "id"))
	fmt.Fprintf(w, "account      %s (subscribed: %s)\n", name, cmp.Or(str(u, "subscribed"), "false"))
	fmt.Fprintf(w, "activities   %s\n", str(u, "activity_count"))
	for _, q := range []struct{ label, key string }{
		{"notifications", "notifications"},
		{"LA updates", "live_activity_updates"},
		{"widget updates", "widget_updates"},
		{"emails", "emails"},
	} {
		if limit := str(u, q.key+"_limit"); limit != "" {
			fmt.Fprintf(w, "%-12s %s/%s\n", q.label, cmp.Or(str(u, q.key+"_used"), "0"), limit)
		}
	}
	if r := str(u, "quota_resets_at"); r != "" {
		fmt.Fprintf(w, "resets       %s\n", shortTime(r))
	}
	// The calling integration key, which /auth/me reports for hlk_ callers.
	if k, ok := u["integration_key"].(map[string]any); ok {
		fmt.Fprintf(w, "key name     %s (%s)\n", str(k, "name"), str(k, "id"))
		fmt.Fprintf(w, "permissions  %s\n", keyPermissions(k))
		if k["activity_slugs"] != nil || k["widget_slugs"] != nil {
			fmt.Fprintf(w, "limited to   activities %s, widgets %s\n", slugList(k, "activity_slugs"), slugList(k, "widget_slugs"))
		}
		if e := str(k, "expires_at"); e != "" {
			fmt.Fprintf(w, "expires      %s\n", shortTime(e))
		}
	}
	return nil
}

func newHealthCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "health",
		Short:       "Check that the API is up (needs no key)",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getHealth"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := a.call(cmd.Context(), "getHealth", nil, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "%s", str(decode(resp.Body), "status"))
		},
	}
}

func newAPICmd(a *App) *cobra.Command {
	var method string
	var headers []string
	var bf bodyFlags
	c := &cobra.Command{
		Use:   "api <path>",
		Short: "Make an authenticated request to any API path",
		Long: `Call an API path directly, for endpoints or fields the other commands do
not cover. Fields go in the query string for GET and DELETE and in a JSON
body otherwise. The method defaults to GET, or POST when a body is given.

  pushward api /activities?state=ongoing
  pushward api -X PATCH /activities/build -F content.progress=0.5
  pushward api /notifications -f title=Hi -f body=There`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			path := argv[0]
			if strings.Contains(path, "://") {
				return usagef("path must be an API path like /activities, not a URL")
			}
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			u, err := url.Parse(path)
			if err != nil || u.Host != "" || u.Scheme != "" {
				return usagef("path must be an API path like /activities, not a URL")
			}
			hasBody := bf.data != "" || len(bf.str) > 0 || len(bf.typed) > 0
			m := strings.ToUpper(method)
			if m == "" {
				m = http.MethodGet
				if hasBody {
					m = http.MethodPost
				}
			}
			req := api.Request{Method: m, Path: u.Path, Query: u.Query(), Header: http.Header{}}
			for _, h := range headers {
				k, v, ok := strings.Cut(h, ":")
				if !ok {
					return usagef("--header %q: want Name: value", h)
				}
				if strings.EqualFold(strings.TrimSpace(k), "Authorization") {
					return usagef("--header: Authorization is set from your key")
				}
				req.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
			if hasBody {
				if m == http.MethodGet || m == http.MethodDelete {
					if bf.data != "" {
						return usagef("--data needs a method with a body (-X POST or PATCH)")
					}
					for _, f := range slices.Concat(bf.str, bf.typed) {
						k, v, _ := strings.Cut(f, "=")
						req.Query.Add(k, v)
					}
				} else {
					obj, err := bf.build(a, nil)
					if err != nil {
						return err
					}
					if req.Body, err = json.Marshal(obj); err != nil {
						return err
					}
				}
			}
			c, err := a.keyed()
			if err != nil {
				return err
			}
			resp, err := c.Do(cmd.Context(), req)
			if err != nil {
				return err
			}
			a.Body = resp.Body
			return a.out().Print(resp.Body, nil)
		},
	}
	c.Flags().StringVarP(&method, "method", "X", "", "HTTP method")
	c.Flags().StringArrayVarP(&headers, "header", "H", nil, "extra header as Name: value (repeatable)")
	bf.register(c)
	return c
}
