package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/output"
)

func newMuteCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "mute",
		Aliases: []string{"mutes"},
		Short:   "Pause notifications and Live Activities on every device",
		Long: `Mute a notification source or an activity slug on every device of the
account: for a while (--for, --until), during a daily window (--schedule, in
--tz or this machine's time zone), or until the mute is removed. The target
is matched exactly. '*' mutes everything except critical alerts; quote it,
or the shell expands it to the files in the current directory. A mute on one
source silences its critical alerts too.

--applies-to picks notifications, activities or both (the default).
--mode silent (the default) delivers notifications without sound, banner or
screen wake, and --mode off sends no push; either way they are kept in the
app's list. While a target is muted no new Live Activity starts for it, and
the running ones update without sound.

Needs a key with the account level: read to list, manage to add and remove.

  pushward mute add '*' --for 1h --label "Deploy"
  pushward mute add backup --applies-to notifications --mode off
  pushward mute add '*' --schedule 22:00-07:00
  pushward mute list
  pushward mute remove <mute-id>`,
	}
	c.AddCommand(newMuteListCmd(a), newMuteAddCmd(a), newMuteRemoveCmd(a))
	return group(c)
}

func newMuteListCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List mutes, oldest first; timed ones that ended are left out",
		Args:        checkArgs,
		Annotations: operation("listMutes"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := a.call(cmd.Context(), "listMutes", nil, nil, nil)
			if err != nil {
				return muteHint(err, "read")
			}
			return a.out().Print(resp.Body, func(w io.Writer) error {
				var list struct {
					Mutes []map[string]any `json:"mutes"`
				}
				if err := json.Unmarshal(resp.Body, &list); err != nil {
					return err
				}
				rows := make([][]string, 0, len(list.Mutes))
				for _, m := range list.Mutes {
					rows = append(rows, []string{str(m, "id"), str(m, "target"), str(m, "applies_to"), str(m, "mode"),
						shortTime(str(m, "until")), muteWindow(m), str(m, "label")})
				}
				return output.Table(w, []string{"ID", "TARGET", "APPLIES TO", "MODE", "UNTIL", "SCHEDULE", "LABEL"}, rows)
			})
		},
	}
}

var muteFields = []field{
	{"applies-to", "applies_to", kString, "notifications, activities or both (the default)"},
	{"mode", "mode", kString, "silent (the default) delivers without sound, banner or screen wake; off sends no push"},
	{"label", "label", kString, "a name for the mute in the app, up to 100 characters"},
}

func newMuteAddCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var dur, until, window, tz string
	c := &cobra.Command{
		Use:   "add <target>",
		Short: "Mute a source or activity slug, or '*' for everything but critical alerts",
		Long: `Mute <target>, a notification source or activity slug matched exactly, or
'*' (quoted) for everything except critical alerts. Without --for, --until
or --schedule the mute lasts until it is removed. A target holds one mute per
--applies-to with --schedule and one without, and adding another replaces
that one under the same id: a second --schedule replaces the first window,
and --for puts an end on a mute that had none. Removing the id that add
printed then ends the mute that was there before, so a script that removes
its own mute checks pushward mute list first. An account holds up to 100
mutes.

  pushward mute add '*' --for 1h --label "Deploy"
  pushward mute add backup --applies-to notifications --mode off --until 2026-12-01T08:00:00+01:00
  pushward mute add '*' --schedule 22:00-07:00 --tz Europe/Warsaw`,
		Args:        checkArgs,
		Annotations: operation("createMute"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectFields(cmd, muteFields, a.Now())
			if err != nil {
				return err
			}
			conv["target"] = argv[0]
			switch {
			case dur != "" && until != "":
				return usagef("--for and --until are mutually exclusive")
			case dur != "":
				s, err := parseSeconds(dur)
				if err != nil {
					return usagef("--for: %v", err)
				}
				if s == 0 {
					return usagef("--for: must be at least 1s")
				}
				conv["until"] = a.Now().Add(time.Duration(s) * time.Second).UTC().Format(time.RFC3339)
			case until != "":
				t, err := time.Parse(time.RFC3339, until)
				if err != nil {
					return usagef("--until: want RFC 3339 like 2026-12-01T08:00:00+01:00")
				}
				conv["until"] = t.Format(time.RFC3339)
			}
			switch {
			case window != "":
				start, end, err := parseWindow(window)
				if err != nil {
					return err
				}
				zone := tz
				if zone == "" {
					if zone, err = a.localZone(); err != nil {
						return err
					}
				}
				conv["schedule"] = map[string]any{"start_minute": start, "end_minute": end, "time_zone": zone}
			case tz != "":
				return usagef("--tz only applies with --schedule")
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if b["applies_to"] == nil {
				b["applies_to"] = "both"
			}
			resp, err := a.call(cmd.Context(), "createMute", nil, nil, b)
			if err != nil {
				return muteHint(err, "manage")
			}
			m := decode(resp.Body)
			line := "muted " + str(m, "target")
			if u := str(m, "until"); u != "" {
				line += " until " + shortTime(u)
			}
			if w := muteWindow(m); w != "" {
				line += " daily " + w
			}
			return a.out().Printf(resp.Body, "%s (%s)", line, str(m, "id"))
		},
	}
	addFields(c, muteFields)
	c.Flags().StringVar(&dur, "for", "", "mute for this long (e.g. 30m, 2h, 1d)")
	c.Flags().StringVar(&until, "until", "", "mute until this time (RFC 3339)")
	c.Flags().StringVar(&window, "schedule", "", "mute every day from HH:MM to HH:MM, 24-hour; an end before the start runs past midnight (22:00-07:00)")
	c.Flags().StringVar(&tz, "tz", "", "IANA time zone of --schedule, like Europe/Warsaw (default: this machine's)")
	bf.register(c)
	return c
}

func newMuteRemoveCmd(a *App) *cobra.Command {
	c := a.deleteCmd("remove <mute-id>", "Remove a mute", "deleteMute", "removed", "mute")
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, argv []string) error {
		return muteHint(run(cmd, argv), "manage")
	}
	return c
}

// muteHint says what to do about the two refusals mute commands meet: a key
// without the account level, and a mute id that is not this account's.
func muteHint(err error, level string) error {
	var ae *api.Error
	switch {
	case !errors.As(err, &ae):
	case ae.Status == http.StatusForbidden && ae.Code == "integration_key.permission_denied":
		return fmt.Errorf("%w\nhint: this needs a personal key with the account level at %s or above. Every key starts without it: raise it in the app (Settings > Manage Keys), "+
			"or from a key that has it with pushward key update <key-id> --account %[2]s; organization keys never have it", err, level)
	case ae.Status == http.StatusNotFound && ae.Code == "mute.not_found":
		return fmt.Errorf("%w\nhint: pushward mute list shows the ids of this account's mutes", err)
	}
	return err
}

// parseWindow reads a --schedule window like 22:00-07:00 into minutes since
// midnight. An end before the start wraps past midnight; an end equal to the
// start is refused, since the server would.
func parseWindow(raw string) (start, end int, err error) {
	from, to, _ := strings.Cut(raw, "-")
	s, errS := time.Parse("15:04", from)
	e, errE := time.Parse("15:04", to)
	if errS != nil || errE != nil {
		return 0, 0, usagef("--schedule: want a daily window like 22:00-07:00, got %q", raw)
	}
	start, end = s.Hour()*60+s.Minute(), e.Hour()*60+e.Minute()
	if start == end {
		return 0, 0, usagef("--schedule: %q starts and ends at the same time; leave --schedule out to mute all day", raw)
	}
	return start, end, nil
}

// muteWindow renders a mute's daily window as 22:00-07:00 Europe/Warsaw.
func muteWindow(m map[string]any) string {
	s, ok := m["schedule"].(map[string]any)
	if !ok {
		return ""
	}
	hm := func(k string) string {
		n, _ := s[k].(float64)
		return fmt.Sprintf("%02d:%02d", int(n)/60, int(n)%60)
	}
	return hm("start_minute") + "-" + hm("end_minute") + " " + str(s, "time_zone")
}

// localtime names this machine's time zone on macOS and most Linux systems,
// as a link into a zoneinfo directory. Tests point it elsewhere.
var localtime = "/etc/localtime"

// localZone is this machine's IANA time zone name, which time.Local does not
// carry: $TZ, else the zone /etc/localtime links to.
func (a *App) localZone() (string, error) {
	name := strings.TrimPrefix(a.Getenv("TZ"), ":")
	if name == "" {
		name, _ = os.Readlink(localtime)
	}
	if _, zone, ok := strings.Cut(name, "zoneinfo/"); ok {
		name = zone
	}
	if name == "" || strings.HasPrefix(name, "/") {
		return "", usagef("--schedule: cannot tell this machine's time zone; pass --tz with an IANA name like Europe/Warsaw")
	}
	return name, nil
}
