package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/callback"
)

func newReceiptCmd(a *App) *cobra.Command {
	var wait time.Duration
	c := group(&cobra.Command{
		Use:     "receipt",
		Aliases: []string{"receipts"},
		Short:   "Follow and cancel notifications that repeat until acknowledged",
		Long: `A notification sent with --ack repeats until someone acknowledges it on a
device or it expires. Its receipt, keyed by the notification id, says where
that stands. pushward receipt <id> is short for pushward receipt get <id>.

  pushward receipt 42 --wait 15m
  pushward receipt cancel 42
  pushward receipt cancel --tag db-1`,
	})
	c.AddCommand(newReceiptGetCmd(a), newReceiptCancelCmd(a), newReceiptSecretCmd(a))
	c.Flags().DurationVar(&wait, "wait", 0, "wait this long for an acknowledgement")
	unknown := c.RunE
	c.RunE = func(cmd *cobra.Command, argv []string) error {
		if len(argv) == 1 && isID(argv[0]) {
			return a.waitReceipt(cmd.Context(), argv[0], wait)
		}
		return unknown(cmd, argv)
	}
	return c
}

func isID(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func newReceiptGetCmd(a *App) *cobra.Command {
	var wait time.Duration
	c := &cobra.Command{
		Use:   "get <id>",
		Short: "Show where a notification sent with --ack stands",
		Long: `Print the receipt of a notification sent with --ack: active while it
repeats, then acknowledged, expired or canceled, and how its callback went.
With --wait it blocks until someone acknowledges it, and exits 7 when it
expires, is canceled or the wait runs out first.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getNotificationReceipt"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.waitReceipt(cmd.Context(), argv[0], wait)
		},
	}
	c.Flags().DurationVar(&wait, "wait", 0, "wait this long for an acknowledgement")
	return c
}

// unacknowledgedError ends a wait on a receipt that finished without an
// acknowledgement. It exits 7 like a timeout: nobody answered.
type unacknowledgedError struct{ status string }

func (e *unacknowledgedError) Error() string {
	return "the notification was " + e.status + " before anyone acknowledged it"
}

func (e *unacknowledgedError) Is(target error) bool { return target == errWaitTimeout }

// waitReceipt reads a receipt, long-polling for up to wait while it is
// active. With no wait, any status is a result rather than a timeout.
func (a *App) waitReceipt(ctx context.Context, id string, wait time.Duration) error {
	last, err := a.poll(ctx, "getNotificationReceipt", map[string]string{"id": id}, wait, func(b []byte) (bool, error) {
		switch s := str(decode(b), "status"); s {
		case "active":
			return false, nil
		case "acknowledged":
			return true, nil
		default:
			return true, &unacknowledgedError{s}
		}
	})
	if last == nil {
		return err
	}
	if wait <= 0 && errors.Is(err, errWaitTimeout) {
		err = nil
	}
	return cmp.Or(a.out().Print(last, func(w io.Writer) error {
		_, err := io.WriteString(w, describeReceipt(decode(last)))
		return err
	}), err)
}

// describeReceipt renders a receipt as one line, plus one for the callback.
func describeReceipt(r map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "notification %s: %s", str(r, "notification_id"), str(r, "status"))
	switch str(r, "status") {
	case "active":
		fmt.Fprintf(&b, ", repeated %s, every %ss until %s", plural(str(r, "repeats_sent"), "time"), str(r, "repeat_seconds"), shortTime(str(r, "expires_at")))
	case "acknowledged":
		b.WriteString(" " + shortTime(str(r, "acknowledged_at")))
		if by := cmp.Or(str(r, "acknowledged_by_device"), str(r, "acknowledged_by")); by != "" {
			b.WriteString(" by " + by)
		}
		if act := str(r, "action_id"); act != "" && act != "pw_ack" {
			b.WriteString(" with " + act)
		}
	case "expired":
		fmt.Fprintf(&b, " %s after %s", shortTime(str(r, "expires_at")), plural(str(r, "repeats_sent"), "repeat"))
	case "canceled":
		b.WriteString(" " + shortTime(str(r, "canceled_at")))
		if why := str(r, "cancel_reason"); why != "" {
			b.WriteString(" (" + why + ")")
		}
	}
	b.WriteByte('\n')
	if cb := str(r, "callback", "status"); cb != "" {
		fmt.Fprintf(&b, "callback %s", cb)
		if n := str(r, "callback", "attempts"); n != "" && n != "0" {
			fmt.Fprintf(&b, " after %s", plural(n, "attempt"))
		}
		if code := str(r, "callback", "last_status_code"); code != "" {
			fmt.Fprintf(&b, " (HTTP %s)", code)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func newReceiptCancelCmd(a *App) *cobra.Command {
	var tag string
	c := &cobra.Command{
		Use:   "cancel [id]",
		Short: "Stop a notification from repeating",
		Long: `Stop the repeats of one notification sent with --ack, or of every active
one with the tag. A tag reaches only what the same integration key sent.
The notifications already delivered stay on the devices; a receipt that
already finished is left as it is.

  pushward receipt cancel 42
  pushward receipt cancel --tag db-1`,
		Args: func(cmd *cobra.Command, got []string) error {
			switch {
			case len(got) > 1:
				return usagef("%s takes one id", cmd.CommandPath())
			case len(got) == 1 && tag != "":
				return usagef("give an id or --tag, not both")
			case len(got) == 0 && tag == "":
				return usagef("%s needs an id or --tag", cmd.CommandPath())
			case len(got) == 1 && !isID(got[0]):
				return usagef("invalid id %q: want a number", got[0])
			}
			return nil
		},
		Annotations: map[string]string{"operation": "cancelNotificationReceipt,cancelNotificationReceiptsByTag"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if tag != "" {
				resp, err := a.call(cmd.Context(), "cancelNotificationReceiptsByTag", nil, nil, map[string]any{"tag": tag})
				if err != nil {
					return err
				}
				return a.out().Printf(resp.Body, "canceled %s tagged %s", plural(str(decode(resp.Body), "canceled"), "notification"), tag)
			}
			resp, err := a.call(cmd.Context(), "cancelNotificationReceipt", map[string]string{"id": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			if s := str(decode(resp.Body), "status"); s != "canceled" {
				return a.out().Printf(resp.Body, "notification %s was already %s", argv[0], s)
			}
			return a.out().Printf(resp.Body, "canceled the repeats of notification %s", argv[0])
		},
	}
	c.Flags().StringVar(&tag, "tag", "", "cancel every active notification this key sent with this tag")
	return c
}

func plural(n, noun string) string {
	if n == "1" {
		return n + " " + noun
	}
	return cmp.Or(n, "0") + " " + noun + "s"
}

func newReceiptSecretCmd(a *App) *cobra.Command {
	var key string
	c := &cobra.Command{
		Use:   "secret",
		Short: "Print the secret callbacks are signed with",
		Long: `Print the whsec_ secret PushWard signs --callback-url requests with, in the
Standard Webhooks format (webhook-id, webhook-timestamp and
webhook-signature headers). It is derived here, with no request, from the
integration key that sends the notification: the configured one, or --key.
Rolling the key changes it. --key - reads the key from stdin, or prompts for
it on a terminal, which keeps it out of shell history.

  op read op://Private/pushward-alerts/credential | pushward receipt secret --key -`,
		Args: checkArgs,
		RunE: func(*cobra.Command, []string) error {
			if key == "-" {
				k, err := a.readSecret("Paste your integration key: ", false)
				if err != nil {
					return err
				}
				key = k
			}
			if key == "" {
				c, err := a.keyed()
				if err != nil {
					return err
				}
				key = c.Token
			}
			key = strings.TrimSpace(key)
			if !strings.HasPrefix(key, "hlk_") {
				return usagef("only integration keys (hlk_...) can set a callback url, so only they have a callback secret")
			}
			secret := callback.Secret(key)
			data, err := json.Marshal(map[string]string{"secret": secret})
			if err != nil {
				return err
			}
			return a.out().Printf(data, "%s", secret)
		},
	}
	c.Flags().StringVar(&key, "key", "", "integration key (hlk_...) to derive it from instead of the configured one, or - for stdin")
	return c
}
