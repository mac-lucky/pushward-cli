package cmd

import (
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

func newScheduleCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "schedule",
		Aliases: []string{"scheduled"},
		Short:   "Schedule notifications for later or on a cron",
	}
	c.AddCommand(newScheduleCreateCmd(a), newScheduleListCmd(a), newScheduleGetCmd(a), newScheduleCancelCmd(a))
	return group(c)
}

func newScheduleCreateCmd(a *App) *cobra.Command {
	var in notificationInput
	var at, in2, cron, tz, until string
	var count int
	c := &cobra.Command{
		Use:   "create",
		Short: "Schedule a notification",
		Long: `Schedule a notification once (--at or --in) or on a cron (--cron with --tz).
Takes the same notification flags as notify.

  pushward schedule create --in 2h --title "Stand up" --body "Stretch"
  pushward schedule create --cron "0 9 * * 1-5" --tz Europe/Warsaw \
    --title "Standup" --body "10 minutes"`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "createScheduledNotification"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := in.build(a, cmd)
			if err != nil {
				return err
			}
			now := a.Now()
			switch {
			case at != "" && in2 != "":
				return usagef("--at and --in are mutually exclusive")
			case at != "":
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return usagef("--at: want RFC 3339 like 2026-10-01T09:00:00+02:00")
				}
				b["send_at"] = t.Format(time.RFC3339)
			case in2 != "":
				s, err := parseSeconds(in2)
				if err != nil {
					return usagef("--in: %v", err)
				}
				b["send_at"] = now.Add(time.Duration(s) * time.Second).UTC().Format(time.RFC3339)
			}
			if cron != "" {
				if tz == "" {
					return usagef("--cron needs --tz, an IANA zone like Europe/Warsaw or UTC")
				}
				rec := map[string]any{"cron": cron, "timezone": tz}
				if until != "" {
					t, err := time.Parse(time.RFC3339, until)
					if err != nil {
						return usagef("--until: want RFC 3339")
					}
					rec["until"] = t.Format(time.RFC3339)
				}
				if count > 0 {
					rec["count"] = count
				}
				b["recurrence"] = rec
			} else if until != "" || count > 0 || tz != "" {
				return usagef("--tz, --until and --count only apply with --cron")
			}
			if b["send_at"] == nil && b["recurrence"] == nil {
				return usagef("say when: --at, --in or --cron")
			}
			resp, err := a.call(cmd.Context(), "createScheduledNotification", nil, nil, b)
			if err != nil {
				return in.explain(err)
			}
			s := decode(resp.Body)
			return a.out().Printf(resp.Body, "scheduled notification %s for %s%s", str(s, "id"), shortTime(str(s, "send_at")), in.sent())
		},
	}
	in.register(c)
	c.Flags().StringVar(&at, "at", "", "send at this time (RFC 3339)")
	c.Flags().StringVar(&in2, "in", "", "send after this long (e.g. 90m, 2h, 1d)")
	c.Flags().StringVar(&cron, "cron", "", "repeat on a 5-field cron expression or @daily, @weekly, ...")
	c.Flags().StringVar(&tz, "tz", "", "IANA time zone the cron runs in (required with --cron)")
	c.Flags().StringVar(&until, "until", "", "last allowed send time for --cron (RFC 3339)")
	c.Flags().IntVar(&count, "count", 0, "stop --cron after this many sends")
	return c
}

func newScheduleListCmd(a *App) *cobra.Command {
	var status string
	var pf pageFlags
	c := &cobra.Command{
		Use:         "list",
		Short:       "List scheduled notifications",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "listScheduledNotifications"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			b, err := a.listPages(cmd, "listScheduledNotifications", q, "cursor", pf)
			if err != nil {
				return err
			}
			return a.out().Items(b, []string{"ID", "STATUS", "SEND AT", "CRON", "TITLE"}, func(it map[string]any) []string {
				return []string{str(it, "id"), str(it, "status"), shortTime(str(it, "send_at")), str(it, "recurrence", "cron"), str(it, "title")}
			})
		},
	}
	c.Flags().StringVar(&status, "status", "", "scheduled (default), sent, failed, canceled or all")
	pf.register(c, "cursor")
	return c
}

func newScheduleGetCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "get <id>",
		Short:       "Show one scheduled notification",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getScheduledNotification"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			resp, err := a.call(cmd.Context(), "getScheduledNotification", map[string]string{"id": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, nil)
		},
	}
}

func newScheduleCancelCmd(a *App) *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:         "cancel <id>",
		Short:       "Cancel a scheduled notification",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "cancelScheduledNotification"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			q := url.Values{}
			if purge {
				q.Set("purge", "true")
			}
			if _, err := a.call(cmd.Context(), "cancelScheduledNotification", map[string]string{"id": argv[0]}, q, nil); err != nil {
				return err
			}
			// A 204 has no body to take the status from.
			a.Status = "canceled"
			a.out().Human("canceled scheduled notification %s", argv[0])
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "delete it outright instead of keeping a canceled record")
	return c
}
