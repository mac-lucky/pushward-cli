package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/body"
)

// notificationInput holds the flags notify and schedule create share.
type notificationInput struct {
	bf        bodyFlags
	bodyText  string
	image     string
	mediaURL  string
	mediaType string
	actions   []string
	meta      []string
	noPush    bool
	encrypt   bool
	noEncrypt bool
	// kid is the key ID build encrypted with, "" when it did not.
	kid string
}

func (n *notificationInput) register(c *cobra.Command) {
	addFields(c, notificationFields)
	c.Flags().StringVar(&n.bodyText, "body", "", "body text, or - to read it from stdin (required)")
	c.Flags().StringVar(&n.image, "image", "", "https image to attach")
	c.Flags().StringVar(&n.mediaURL, "media-url", "", "https media to attach (with --media-type)")
	c.Flags().StringVar(&n.mediaType, "media-type", "", "media type: image, video or audio")
	c.Flags().StringArrayVar(&n.actions, "action", nil, "answer button as id=Title (repeatable); makes the notification answerable")
	c.Flags().StringArrayVar(&n.meta, "meta", nil, "metadata key=value (repeatable)")
	c.Flags().BoolVar(&n.noPush, "no-push", false, "store in the inbox without pushing to devices")
	c.Flags().BoolVar(&n.encrypt, "encrypt", false, "encrypt title, subtitle, body and url, failing when no encryption key is set (the default whenever one is)")
	c.Flags().BoolVar(&n.noEncrypt, "no-encrypt", false, "send this one unencrypted even though an encryption key is set")
	n.bf.register(c)
}

func (n *notificationInput) build(a *App, cmd *cobra.Command) (map[string]any, error) {
	conv, err := collectFields(cmd, notificationFields, a.Now())
	if err != nil {
		return nil, err
	}
	if n.bodyText == "-" {
		b, _, err := body.Read("-", a.Stdin)
		if err != nil {
			return nil, usagef("--body: %v", err)
		}
		conv["body"] = strings.TrimRight(string(b), "\n")
	} else if n.bodyText != "" {
		conv["body"] = n.bodyText
	}
	switch {
	case n.image != "" && n.mediaURL != "":
		return nil, usagef("--image and --media-url are mutually exclusive")
	case n.image != "":
		conv["media"] = map[string]any{"url": n.image, "type": "image"}
	case n.mediaURL != "":
		if n.mediaType == "" {
			return nil, usagef("--media-url needs --media-type image, video or audio")
		}
		conv["media"] = map[string]any{"url": n.mediaURL, "type": n.mediaType}
	}
	if len(n.actions) > 0 {
		if conv["actions"], err = buttons("action", n.actions); err != nil {
			return nil, err
		}
	}
	if len(n.meta) > 0 {
		ps, err := pairs("meta", n.meta)
		if err != nil {
			return nil, err
		}
		m := map[string]any{}
		for _, p := range ps {
			m[p[0]] = p[1]
		}
		conv["metadata"] = m
	}
	if n.noPush {
		conv["push"] = false
	}
	b, err := n.bf.build(a, conv)
	if err != nil {
		return nil, err
	}
	// Encrypt last, so text from --data, -f and -F is sealed too.
	switch {
	case n.encrypt && n.noEncrypt:
		return nil, usagef("--encrypt and --no-encrypt are mutually exclusive")
	case !n.noEncrypt:
		if n.kid, err = a.encryptBody(b, n.encrypt); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// sent describes how the notification went out, for the summary line.
func (n *notificationInput) sent() string {
	if n.kid == "" {
		return ""
	}
	return " (encrypted, key ID " + n.kid + ")"
}

// explain adds the way out to an error a configured key caused: the server
// refuses encrypted notifications from organization keys.
func (n *notificationInput) explain(err error) error {
	var ae *api.Error
	if n.kid != "" && !n.encrypt && errors.As(err, &ae) && ae.Code == api.CodeEncryptionUnavailable {
		return fmt.Errorf("%w\n  an encryption key is set; pass --no-encrypt to send this one unencrypted", err)
	}
	return err
}

func newNotifyCmd(a *App) *cobra.Command {
	return notifyCommand(a, "notify", "Send a push notification")
}

func notifyCommand(a *App, use, short string) *cobra.Command {
	var in notificationInput
	var wait time.Duration
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Long: `Send a push notification.

  pushward notify --title "Backup done" --body "412 GB in 38m"
  make 2>&1 | tail -20 | pushward notify --title "Build log" --body -

With --action the notification gets buttons, and --wait blocks until one is
tapped, then prints the answer (exit 7 if nobody answers in time):

  pushward notify --title "Deploy to prod?" --body "v2.4.0" \
    --action deploy=Deploy --action skip=Skip --wait 15m --jq .action_id`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "createNotification"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := in.build(a, cmd)
			if err != nil {
				return err
			}
			if wait > 0 && b["actions"] == nil {
				return usagef("--wait needs an --action to answer with")
			}
			ctx := cmd.Context()
			resp, err := a.call(ctx, "createNotification", nil, nil, b)
			if err != nil {
				return in.explain(err)
			}
			n := decode(resp.Body)
			id := str(n, "id")
			if d := str(n, "delivery"); d != "" && d != "all" {
				a.out().Warnf("delivery %s (%s)", d, cmp.Or(str(n, "reason"), "no reason given"))
			}
			if wait <= 0 {
				return a.out().Printf(resp.Body, "sent notification %s%s", id, in.sent())
			}
			// Actions that all carry a url are dispatched by the device and
			// never reach the server as answers.
			if str(n, "answerable") != "true" {
				return usagef("notification %s cannot be answered: --wait needs at least one --action without a url", id)
			}
			a.out().Human("sent notification %s%s, waiting for an answer", id, in.sent())
			return a.waitAnswer(ctx, id, wait)
		},
	}
	in.register(c)
	c.Flags().DurationVar(&wait, "wait", 0, "after sending, wait this long for an answer and print it")
	return c
}

func newNotificationCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "notification",
		Aliases: []string{"notifications"},
		Short:   "Send notifications and read their answers",
	}
	c.AddCommand(
		notifyCommand(a, "send", "Send a push notification (same as pushward notify)"),
		newAnswerCmd(a),
	)
	return group(c)
}

func newAnswerCmd(a *App) *cobra.Command {
	var wait time.Duration
	c := &cobra.Command{
		Use:   "answer <id>",
		Short: "Read the answer to a notification",
		Long: `Read which action was tapped on a notification. Without --wait it prints
the current status (pending or answered). With --wait it blocks until an
answer arrives and exits 7 if none does in time.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getNotificationAnswer"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.waitAnswer(cmd.Context(), argv[0], wait)
		},
	}
	c.Flags().DurationVar(&wait, "wait", 0, "wait this long for an answer")
	return c
}

// waitAnswer reads a notification's answer, long-polling for up to wait.
// With no wait, a pending answer is a result rather than a timeout.
func (a *App) waitAnswer(ctx context.Context, id string, wait time.Duration) error {
	var ans map[string]any
	last, err := a.poll(ctx, "getNotificationAnswer", map[string]string{"id": id}, wait, func(b []byte) (bool, error) {
		ans = decode(b)
		return str(ans, "status") == "answered", nil
	})
	if last == nil {
		return err
	}
	if wait <= 0 && errors.Is(err, errWaitTimeout) {
		err = nil
	}
	perr := a.out().Print(last, func(w io.Writer) error {
		if str(ans, "status") != "answered" {
			_, err := fmt.Fprintln(w, "no answer yet")
			return err
		}
		line := "answered: " + str(ans, "action_id")
		if t := str(ans, "text"); t != "" {
			line += fmt.Sprintf(" (%q)", t)
		}
		_, err := fmt.Fprintln(w, line)
		return err
	})
	return cmp.Or(perr, err)
}
