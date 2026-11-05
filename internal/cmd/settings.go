package cmd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// The account's own settings an integration key can reach, at the devices,
// sharing and account levels: devices and what they follow, shares, the
// nickname, onboarding and email recipients.

func newDeviceCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "device",
		Aliases: []string{"devices"},
		Short:   "Your devices and the activities they follow",
		Long: `List, rename and remove the account's devices, and choose which activities
each one follows. Needs a key with the devices level (read to list, manage
to change). Devices register themselves from the app; this cannot add one.

  pushward device list
  pushward device follow <device-id> deploy backup
  pushward device follows <device-id> --activity deploy`,
	}
	c.AddCommand(
		a.listCmd("list", "List devices, most recently active first", "listDevices", 0,
			[]string{"DEVICE ID", "NAME", "PLATFORM", "MODEL", "APP", "LAST ACTIVE", "CURRENT"}, func(it map[string]any) []string {
				cur := ""
				if c, _ := it["is_current"].(bool); c {
					cur = "yes"
				}
				return []string{str(it, "device_id"), str(it, "name"), str(it, "platform"), str(it, "device_model"),
					str(it, "app_version"), shortTime(str(it, "last_active_at")), cur}
			}),
		a.getCmd("get <device-id>", "Show one device", "getDevice"),
		a.writeCmd("rename <device-id> <name>", "Rename a device (1-64 characters)", "updateDevice", nil, "renamed the device"),
		a.deleteCmd("delete <device-id>", "Remove a device and sign it out", "deleteDevice", "removed", "device"),
		newDeviceFollowsCmd(a),
		newDeviceFollowCmd(a, "follow", "subscribeDevice", "Make a device follow activities", "follows"),
		newDeviceFollowCmd(a, "unfollow", "unsubscribeDevice", "Stop a device following activities", "no longer follows"),
	)
	return group(c)
}

func newDeviceFollowsCmd(a *App) *cobra.Command {
	var slug string
	c := &cobra.Command{
		Use:   "follows <device-id>",
		Short: "List the activities a device follows, or check one with --activity",
		Args:  checkArgs,
		// --activity picks the single-activity check.
		Annotations: operation("listSubscriptions,getDeviceSubscription"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			params := map[string]string{"device_id": argv[0]}
			if slug != "" {
				if !slugPattern.MatchString(slug) {
					return usagef("--activity: invalid slug %q", slug)
				}
				params["slug"] = slug
				resp, err := a.call(cmd.Context(), "getDeviceSubscription", params, nil, nil)
				if err != nil {
					return err
				}
				on, _ := decode(resp.Body)["is_subscribed"].(bool)
				return a.out().Printf(resp.Body, "%s follows %s: %v", argv[0], slug, on)
			}
			resp, err := a.call(cmd.Context(), "listSubscriptions", params, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, func(w io.Writer) error {
				var subs struct {
					Items []string `json:"items"`
				}
				if err := json.Unmarshal(resp.Body, &subs); err != nil {
					return err
				}
				for _, s := range subs.Items {
					fmt.Fprintln(w, s)
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&slug, "activity", "", "only check whether it follows this activity slug")
	return c
}

func newDeviceFollowCmd(a *App, name, opID, short, done string) *cobra.Command {
	return &cobra.Command{
		Use:         name + " <device-id> <slug>...",
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			for _, slug := range argv[1:] {
				if _, err := a.call(cmd.Context(), opID, map[string]string{"device_id": argv[0], "slug": slug}, nil, nil); err != nil {
					return fmt.Errorf("%s: %w", slug, err)
				}
			}
			a.out().Human("%s %s %s", argv[0], done, strings.Join(argv[1:], ", "))
			return nil
		},
	}
}

func newShareCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "share",
		Aliases: []string{"shares"},
		Short:   "Share activities with other PushWard users",
		Long: `Share an activity, or every activity matching a pattern, with another user
or through a link they redeem in the app. Needs a key with the sharing level
(read to list, manage to change); granting needs a subscription.

  pushward share grant deploy --role viewer --expires-in 24h --jq .grantee.share_link.share_url
  pushward share grant-pattern 'ci-*' --role viewer
  pushward share list --direction received`,
	}
	c.AddCommand(newShareListCmd(a), newShareGrantCmd(a), newShareGrantPatternCmd(a),
		a.writeCmd("update <share-id>", "Change a share's role", "updatePermission",
			[]field{{"role", "role", kString, "viewer (read-only) or editor (can update)"}}, "share %s is now %s", "id", "role"),
		a.deleteCmd("revoke <share-id>", "Revoke a share, or leave one shared with you", "deletePermission", "revoked", "share"),
	)
	return group(c)
}

func newShareListCmd(a *App) *cobra.Command {
	var slug, direction string
	c := &cobra.Command{
		Use:   "list",
		Short: "List shares you granted, or with --direction received the ones you got",
		Args:  checkArgs,
		// --activity lists the shares of one activity.
		Annotations: operation("listPermissions,listActivityPermissions"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			op, params, q := "listPermissions", map[string]string{}, url.Values{}
			switch {
			case slug != "" && direction != "":
				return usagef("--activity and --direction cannot be used together")
			case slug != "":
				if !slugPattern.MatchString(slug) {
					return usagef("--activity: invalid slug %q", slug)
				}
				op, params["slug"] = "listActivityPermissions", slug
			case direction != "":
				q.Set("direction", direction)
			}
			resp, err := a.call(cmd.Context(), op, params, q, nil)
			if err != nil {
				return err
			}
			return a.out().Items(resp.Body, []string{"ID", "ROLE", "ON", "GRANTEE", "OWNER", "CREATED"}, func(it map[string]any) []string {
				return []string{str(it, "id"), str(it, "role"), cmp.Or(str(it, "activity_slug"), str(it, "slug_pattern")),
					grantee(it), cmp.Or(str(it, "owner_nickname"), str(it, "owner_id")), shortTime(str(it, "created_at"))}
			})
		},
	}
	c.Flags().StringVar(&slug, "activity", "", "only the shares of this activity slug")
	c.Flags().StringVar(&direction, "direction", "", "granted (default: what you handed out) or received (shared with you)")
	return c
}

// grantee renders who a share goes to: a user, or a link with its uses.
func grantee(it map[string]any) string {
	g, _ := it["grantee"].(map[string]any)
	switch t := str(g, "type"); t {
	case "user":
		return "user " + cmp.Or(str(g, "user", "nickname"), str(g, "user", "id"))
	case "share_link", "pattern_link":
		return fmt.Sprintf("link %s/%s uses", cmp.Or(str(g, t, "use_count"), "0"), cmp.Or(str(g, t, "max_uses"), "any"))
	default:
		return t
	}
}

var shareLinkFields = []field{
	{"role", "role", kString, "viewer (read-only, the default) or editor (can update)"},
	{"expires-in", "expires_in", kSeconds, "how long a link can be redeemed: 60s to 7d (default 24h)"},
	{"max-uses", "max_uses", kInt, "how many times a link can be redeemed"},
}

// printShare prints a new share; a link's URL gets its own line.
func (a *App) printShare(body []byte) error {
	m := decode(body)
	return a.out().Print(body, func(w io.Writer) error {
		on := cmp.Or(str(m, "activity_slug"), str(m, "slug_pattern"))
		fmt.Fprintf(w, "shared %s as %s with %s (%s)\n", on, str(m, "role"), grantee(m), str(m, "id"))
		for _, t := range []string{"share_link", "pattern_link"} {
			if u := str(m, "grantee", t, "share_url"); u != "" {
				fmt.Fprintf(w, "%s\nexpires %s\n", u, shortTime(str(m, "grantee", t, "expires_at")))
			}
		}
		return nil
	})
}

func newShareGrantCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var user string
	c := &cobra.Command{
		Use:   "grant <slug>",
		Short: "Share an activity with a user, or as a link",
		Long: `Share an activity with a user by id (--user), or without --user as a link
that anyone who has it can redeem in the app. The link is in the response,
--jq .grantee.share_link.share_url prints just that.`,
		Args:        checkArgs,
		Annotations: operation("grantActivityPermission"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectFields(cmd, shareLinkFields, a.Now())
			if err != nil {
				return err
			}
			g := map[string]any{"type": "share_link"}
			if user != "" {
				if conv["expires_in"] != nil || conv["max_uses"] != nil {
					return usagef("--expires-in and --max-uses are for links, not --user")
				}
				g = map[string]any{"type": "user", "user_id": user}
			}
			for _, k := range []string{"expires_in", "max_uses"} {
				if v, ok := conv[k]; ok {
					g[k] = v
					delete(conv, k)
				}
			}
			conv["grantee"] = g
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "grantActivityPermission", map[string]string{"slug": argv[0]}, nil, b)
			if err != nil {
				return err
			}
			return a.printShare(resp.Body)
		},
	}
	addFields(c, shareLinkFields)
	c.Flags().StringVar(&user, "user", "", "share with this user id instead of making a link")
	bf.register(c)
	return c
}

func newShareGrantPatternCmd(a *App) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:   "grant-pattern <slug-pattern>",
		Short: "Share every activity matching a trailing-* pattern, as a link",
		Long: `Share the activities whose slug matches a pattern like ci-*, now and later
ones, through a link. The link is in the response;
--jq .grantee.pattern_link.share_url prints just that.`,
		Args:        checkArgs,
		Annotations: operation("grantPatternPermission"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectFields(cmd, shareLinkFields, a.Now())
			if err != nil {
				return err
			}
			conv["slug_pattern"] = argv[0]
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "grantPatternPermission", nil, nil, b)
			if err != nil {
				return err
			}
			return a.printShare(resp.Body)
		},
	}
	addFields(c, shareLinkFields)
	bf.register(c)
	return c
}

func newMeUpdateCmd(a *App) *cobra.Command {
	return a.writeCmd("update", "Change your nickname", "updateMe",
		[]field{{"nickname", "nickname", kString, "display name, up to 50 characters; an empty value clears it"}}, "updated the profile")
}

func newMeOnboardingCmd(a *App) *cobra.Command {
	get := a.getCmd("get", "Show the setup state and the setup snippets", "getOnboarding")
	c := &cobra.Command{
		Use:   "onboarding",
		Short: "Show or change the setup state the app's onboarding keeps",
		Long: `Show the account's setup state: what was tried so far and the setup
snippets per source, with {{KEY}} where a key goes. update records the
source picked and turns the setup reminders on or off.

  pushward me onboarding
  pushward me onboarding update --nudges false`,
		Args:        checkArgs,
		Annotations: operation("getOnboarding"),
		RunE:        get.RunE,
	}
	c.AddCommand(get, a.writeCmd("update", "Pick the setup source or turn the setup reminders on or off", "updateOnboarding", []field{
		{"use-case", "use_case", kString, "the source being set up: a sources id from onboarding get, or other"},
		{"nudges", "nudges_enabled", kBool, "true allows the setup reminders, false stops them"},
	}, "updated onboarding"))
	return c
}

func newEmailRecipientCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "recipient",
		Aliases: []string{"recipients"},
		Short:   "Addresses email send may write to",
		Long: `Register the addresses email send may write to. Each one gets a
confirmation link and can receive mail once it is confirmed. Needs a key
with the account level, or an organization key that may send email (for
the organization's own recipients).

  pushward email recipient add ops@example.com --label "Ops inbox"
  pushward email recipient list`,
	}
	c.AddCommand(
		a.listCmd("list", "List recipients", "listEmailRecipients", 0,
			[]string{"ID", "ADDRESS", "STATUS", "LABEL", "CREATED"}, func(it map[string]any) []string {
				return []string{str(it, "id"), str(it, "address"), str(it, "status"), str(it, "label"), shortTime(str(it, "created_at"))}
			}),
		a.writeCmd("add <address>", "Register an address and mail it a confirmation link", "addEmailRecipient",
			[]field{{"label", "label", kString, "a name for the address, up to 128 characters"}}, "added %s (%s): it gets an email with a link to confirm", "address", "status"),
		a.deleteCmd("remove <recipient-id>", "Remove a recipient", "deleteEmailRecipient", "removed", "recipient"),
		a.writeCmd("resend <recipient-id>", "Mail a pending recipient a new confirmation link", "resendEmailVerification", nil,
			"sent %s a new confirmation link", "address"),
	)
	return group(c)
}
