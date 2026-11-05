package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/config"
)

func newOrgCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "org",
		Aliases: []string{"orgs"},
		Short:   "Administer an organization with its org admin key",
		Long: `Org commands send the org admin key (hlo_...), never the integration key.
An owner or admin creates one in the PushWard console. At level read it sees
what a viewer sees (not invites or keys); at manage it acts as an admin.
The key belongs to one organization, so no command takes an org id.

PUSHWARD_ORG_API_TOKEN, when set, wins over the key stored by
pushward auth login --org.

  pushward org get
  pushward org member invite ops@example.com --role member
  pushward org key create ci --notifications schedule --jq .key
  pushward org audit list --limit 20`,
	}
	c.AddCommand(
		newOrgGetCmd(a),
		a.writeCmd("update", "Rename the organization", "updateOrg",
			[]field{{"name", "name", kString, "new name, 1-50 characters"}}, "renamed the organization to %s", "name"),
		newOrgMemberCmd(a),
		newOrgInviteCmd(a),
		newOrgKeyCmd(a),
		newOrgGroupCmd(a),
		newOrgTagCmd(a),
		newOrgRouteCmd(a),
		newOrgDeviceCmd(a),
		newOrgAuditCmd(a),
		newOrgBillingCmd(a),
	)
	return group(c)
}

func newOrgGetCmd(a *App) *cobra.Command {
	c := a.getCmd("get", "Show the organization: plan, status and members", "getOrg")
	// GET /orgs finds the organization first, as for every org command.
	c.Annotations["operation"] = "listOrgs,getOrg"
	return c
}

var orgRoles = "owner, admin, billing, member or viewer"

func newOrgMemberCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "member",
		Aliases: []string{"members"},
		Short:   "List members, invite them by email, change roles",
	}
	c.AddCommand(
		a.listCmd("list", "List members", "listOrgMembers", 1000,
			[]string{"USER ID", "NICKNAME", "ROLE", "EMAIL", "JOINED"}, func(it map[string]any) []string {
				return []string{str(it, "user_id"), str(it, "nickname"), str(it, "role"), str(it, "email"), shortTime(str(it, "joined_at"))}
			}),
		a.getCmd("get <user-id>", "Show one member", "getOrgMember"),
		newOrgMemberInviteCmd(a),
		newOrgMemberUpdateCmd(a),
		a.deleteCmd("remove <user-id>", "Remove a member (your own user id leaves the organization)", "deleteOrgMember", "removed", "member"),
	)
	return group(c)
}

var inviteFields = []field{
	{"role", "role", kString, "role they join with: admin, billing, member or viewer (required)"},
	{"note", "note", kString, "who the invite is for, 1-100 characters"},
}

// inviteBody builds an invite from the flags, with --expires-in in the whole
// hours the API takes.
func inviteBody(a *App, cmd *cobra.Command, fields []field, bf *bodyFlags, expiresIn string, extra map[string]any) (map[string]any, error) {
	conv, err := collectFields(cmd, fields, a.Now())
	if err != nil {
		return nil, err
	}
	if expiresIn != "" {
		s, err := parseSeconds(expiresIn)
		if err != nil || s == 0 || s%3600 != 0 {
			return nil, usagef("--expires-in: want whole hours like 48h or 7d, got %q", expiresIn)
		}
		conv["expires_in_hours"] = s / 3600
	}
	for k, v := range extra {
		conv[k] = v
	}
	b, err := bf.build(a, conv)
	if err != nil {
		return nil, err
	}
	if b["role"] == nil {
		return nil, usagef("--role is required: admin, billing, member or viewer")
	}
	return b, nil
}

func newOrgMemberInviteCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var expiresIn string
	c := &cobra.Command{
		Use:   "invite <email>",
		Short: "Mail someone an invite to join",
		Long: `Mail an invite to an address. It is one-use, and the code goes only into
the mail, so nothing here prints it; the invite stays pending until they
join. The address becomes their label in the organization; it is not how
they sign in.

  pushward org member invite ops@example.com --role member --note "Ops rota"`,
		Args:        checkArgs,
		Annotations: operation("createOrgInvite"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			b, err := inviteBody(a, cmd, inviteFields, &bf, expiresIn, map[string]any{"email": argv[0]})
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createOrgInvite", nil, nil, b)
			if err != nil {
				return err
			}
			m := decode(resp.Body)
			return a.out().Printf(resp.Body, "invited %s as %s: pending until they join (invite %s, expires %s)",
				str(m, "email"), str(m, "role"), str(m, "id"), shortTime(str(m, "expires_at")))
		},
	}
	addFields(c, inviteFields)
	c.Flags().StringVar(&expiresIn, "expires-in", "", "how long the invite works, in whole hours: 48h, 7d (default 7d, at most 30d)")
	bf.register(c)
	return c
}

func newOrgMemberUpdateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var clearEmail bool
	fields := []field{
		{"role", "role", kString, "new role: " + orgRoles},
		{"email", "email", kString, "email label for the member, unique in the organization (not verified, not how they sign in)"},
	}
	c := &cobra.Command{
		Use:         "update <user-id>",
		Short:       "Change a member's role or email label",
		Args:        checkArgs,
		Annotations: operation("updateOrgMember"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectFields(cmd, fields, a.Now())
			if err != nil {
				return err
			}
			if clearEmail {
				if _, set := conv["email"]; set {
					return usagef("--clear-email and --email cannot be used together")
				}
				conv["email"] = nil
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 {
				return usagef("nothing to update: set --role, --email or --clear-email")
			}
			resp, err := a.call(cmd.Context(), "updateOrgMember", map[string]string{"user_id": argv[0]}, nil, b)
			if err != nil {
				return err
			}
			m := decode(resp.Body)
			return a.out().Printf(resp.Body, "updated %s: %s", str(m, "user_id"), str(m, "role"))
		},
	}
	addFields(c, fields)
	c.Flags().BoolVar(&clearEmail, "clear-email", false, "remove the member's email label")
	bf.register(c)
	return c
}

func newOrgInviteCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "invite",
		Aliases: []string{"invites"},
		Short:   "Invite codes: create, list and revoke",
	}
	c.AddCommand(
		a.listCmd("list", "List invites, newest first, revoked and expired ones too", "listOrgInvites", 1000,
			[]string{"ID", "ROLE", "STATE", "USES", "EMAIL", "NOTE", "EXPIRES"}, func(it map[string]any) []string {
				return []string{str(it, "id"), str(it, "role"), inviteState(it, a.Now()), str(it, "uses") + "/" + str(it, "max_uses"),
					str(it, "email"), str(it, "note"), shortTime(str(it, "expires_at"))}
			}),
		a.getCmd("get <invite-id>", "Show one invite (never its code)", "getOrgInvite"),
		newOrgInviteCreateCmd(a),
		a.deleteCmd("revoke <invite-id>", "Revoke an invite", "revokeOrgInvite", "revoked", "invite"),
	)
	return group(c)
}

// inviteState is what the invite list shows for an invite: pending until it
// is used up, expires or is revoked.
func inviteState(it map[string]any, now time.Time) string {
	uses, _ := it["uses"].(float64)
	most, _ := it["max_uses"].(float64)
	exp, err := time.Parse(time.RFC3339, str(it, "expires_at"))
	switch {
	case str(it, "revoked_at") != "":
		return "revoked"
	case most > 0 && uses >= most:
		return "used"
	case err == nil && !exp.After(now):
		return "expired"
	}
	return "pending"
}

func newOrgInviteCreateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var expiresIn string
	fields := append(slices.Clone(inviteFields), field{"max-uses", "max_uses", kInt, "how many people may join with it (default 1)"})
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an invite code to pass on",
		Long: `Create an invite and print its code, which is shown only once; --jq .code
prints just the code. To mail an invite to someone instead, use
pushward org member invite.

  pushward org invite create --role member --max-uses 5 --expires-in 48h --note "Ops rota"`,
		Args:        checkArgs,
		Annotations: operation("createOrgInvite"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.quiet {
				return usagef("--quiet would discard the invite code, which is shown only once; use --jq .code to print just the code")
			}
			b, err := inviteBody(a, cmd, fields, &bf, expiresIn, nil)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createOrgInvite", nil, nil, b)
			if err != nil {
				return err
			}
			m := decode(resp.Body)
			return a.out().Print(resp.Body, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "created invite %s (%s, %s uses, expires %s)\n%s\nPass the code on now: it is not shown again.\n",
					str(m, "id"), str(m, "role"), str(m, "max_uses"), shortTime(str(m, "expires_at")), str(m, "code"))
				return err
			})
		},
	}
	addFields(c, fields)
	c.Flags().StringVar(&expiresIn, "expires-in", "", "how long the invite works, in whole hours: 48h, 7d (default 7d, at most 30d)")
	bf.register(c)
	return c
}

func newOrgKeyCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "key",
		Aliases: []string{"keys"},
		Short:   "The organization's integration keys (hlk_...) that send to members",
		Long: `Manage the organization's sending keys. What they send reaches the members
the organization's routing rules allow, or only the groups and tags given
with --target-group-ids and --target-tag-ids at creation.

Each key has a level per resource: --activities none|read|update|manage,
--notifications none|send|schedule, --widgets none|read|write and
--emails none|send. Without any, a new key manages activities and schedules
notifications; once one is given, a resource you leave out gets none.

  pushward org key create ci --activities manage --notifications schedule --jq .key
  pushward org key update 0b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11 --widgets write
  pushward org key revoke 0b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11`,
	}
	c.AddCommand(
		a.listCmd("list", "List the organization's integration keys", "listOrgKeys", 0,
			[]string{"ID", "NAME", "PERMISSIONS", "ACTIVITIES", "WIDGETS", "TARGET", "EXPIRES", "LAST USED"}, func(it map[string]any) []string {
				return []string{
					str(it, "id"), str(it, "name"), keyPermissions(it),
					slugList(it, "activity_slugs"), slugList(it, "widget_slugs"), keyTarget(it),
					shortTime(str(it, "expires_at")), shortTime(str(it, "last_used_at")),
				}
			}),
		a.getCmd("get <key-id>", "Show one key", "getOrgKey"),
		newOrgKeyCreateCmd(a),
		newOrgKeyUpdateCmd(a),
		newOrgKeyRollCmd(a),
		a.deleteCmd("revoke <key-id>", "Revoke a key", "revokeOrgKey", "revoked", "key"),
	)
	return group(c)
}

// keyTarget renders the groups and tags an organization key is limited to.
func keyTarget(it map[string]any) string {
	t, _ := it["target"].(map[string]any)
	var out []string
	if g := idList(t, "group_ids"); g != "" {
		out = append(out, "groups:"+g)
	}
	if tags := idList(t, "tag_ids"); tags != "" {
		out = append(out, "tags:"+tags)
	}
	if len(out) == 0 {
		return "all"
	}
	return strings.Join(out, " ")
}

// idList renders a list of ids comma-separated.
func idList(m map[string]any, key string) string {
	ids, _ := m[key].([]any)
	list := make([]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, fmt.Sprint(id))
	}
	return strings.Join(list, ",")
}

var orgKeyTargetFields = []field{
	{"target-group-ids", "target.group_ids", kNames, "limit the key to these groups (comma-separated ids); set once, at creation"},
	{"target-tag-ids", "target.tag_ids", kNames, "limit the key to these device tags (comma-separated ids); set once, at creation"},
}

func addOrgKeyFlags(c *cobra.Command) {
	addFields(c, keyFields[1:]) // scope is the personal keys' legacy form
	for _, k := range orgKeyLevels {
		c.Flags().String(k.flag, "", fmt.Sprintf("%s level: %s", k.flag, strings.Join(k.levels, ", ")))
	}
}

// collectOrgKeyFields is collectKeyFields without the legacy fields, which
// organization keys do not take.
func collectOrgKeyFields(cmd *cobra.Command, a *App, fields []field) (map[string]any, error) {
	conv, err := collectFields(cmd, slices.Concat(keyFields[1:], fields), a.Now())
	if err != nil {
		return nil, err
	}
	levels := map[string]any{}
	for _, k := range orgKeyLevels {
		if !cmd.Flags().Changed(k.flag) {
			continue
		}
		v, _ := cmd.Flags().GetString(k.flag)
		if !slices.Contains(k.levels, v) {
			return nil, usagef("--%s: want one of %s, got %q", k.flag, strings.Join(k.levels, ", "), v)
		}
		levels[k.flag] = v
	}
	if len(levels) > 0 {
		conv["permissions"] = levels
	}
	return conv, nil
}

func newOrgKeyCreateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an organization integration key",
		Long: `Create a key that sends as the organization and print its secret, which
is shown only once; --jq .key prints just the secret. A key limited to
groups and tags cannot send emails.`,
		Args:        checkArgs,
		Annotations: operation("createOrgKey"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if err := a.errQuietSecret(); err != nil {
				return err
			}
			conv, err := collectOrgKeyFields(cmd, a, orgKeyTargetFields)
			if err != nil {
				return err
			}
			conv["name"] = argv[0]
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createOrgKey", nil, nil, b)
			if err != nil {
				return err
			}
			return a.printKeySecret(resp.Body, "created")
		},
	}
	addOrgKeyFlags(c)
	addFields(c, orgKeyTargetFields)
	bf.register(c)
	return c
}

func newOrgKeyUpdateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var clears keyClears
	nameField := []field{{"name", "name", kString, "new name"}}
	c := &cobra.Command{
		Use:   "update <key-id>",
		Short: "Change an organization key's name, levels, slugs or expiry",
		Long: `Change a key. Flags you leave out keep their stored value, and a level
flag changes only that resource; --all-activities and --all-widgets remove a
slug restriction and --no-expiry removes the expiry. The target groups and
tags cannot change.`,
		Args:        checkArgs,
		Annotations: operation("updateOrgKey"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectOrgKeyFields(cmd, a, nameField)
			if err != nil {
				return err
			}
			if err := clears.apply(conv); err != nil {
				return err
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 {
				return usagef("nothing to update: set a flag, -f/-F or --data")
			}
			resp, err := a.call(cmd.Context(), "updateOrgKey", map[string]string{"key_id": argv[0]}, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "updated key %s", argv[0])
		},
	}
	addOrgKeyFlags(c)
	addFields(c, nameField)
	clears.register(c)
	bf.register(c)
	return c
}

func newOrgKeyRollCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "roll <key-id>",
		Short: "Replace an organization key's secret",
		Long: `Replace a key's secret and print the new one, which is shown only once.
The old secret stops working at once.`,
		Args:        checkArgs,
		Annotations: operation("rollOrgKey"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if err := a.errQuietSecret(); err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "rollOrgKey", map[string]string{"key_id": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			return a.printKeySecret(resp.Body, "rolled")
		},
	}
}

var describeField = field{"description", "description", kString, "what it is for; an empty value clears it on update"}

func newOrgGroupCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "group",
		Aliases: []string{"groups"},
		Short:   "Member groups that sends and routing rules target",
	}
	c.AddCommand(
		a.listCmd("list", "List groups", "listOrgGroups", 0,
			[]string{"ID", "NAME", "MEMBERS", "DESCRIPTION"}, func(it map[string]any) []string {
				ids, _ := it["member_ids"].([]any)
				return []string{str(it, "id"), str(it, "name"), fmt.Sprint(len(ids)), str(it, "description")}
			}),
		a.getCmd("get <group-id>", "Show one group with its member ids", "getOrgGroup"),
		a.writeCmd("create <name>", "Create a group (lowercase letters, digits, - and _)", "createOrgGroup",
			[]field{describeField}, "created group %s (%s)", "name", "id"),
		a.writeCmd("update <group-id>", "Rename or describe a group", "updateOrgGroup",
			[]field{{"name", "name", kString, "new name"}, describeField}, "updated group %s", "name"),
		a.deleteCmd("delete <group-id>", "Delete a group", "deleteOrgGroup", "deleted", "group"),
		newOrgSetCmd(a, "set-members <group-id> [<user-id>...]", "Replace a group's members (none empties it)", "setOrgGroupMembers", "user_ids",
			"group %s has %d members"),
	)
	return group(c)
}

// newOrgSetCmd replaces a list with the trailing positionals.
func newOrgSetCmd(a *App, use, short, opID, key, done string) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			n := len(positionals(cmd))
			params, _ := splitArgs(cmd, opID, argv[:n])
			list := argv[n:]
			if list == nil {
				list = []string{}
			}
			resp, err := a.call(cmd.Context(), opID, params, nil, map[string]any{key: list})
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, done, argv[n-1], len(list))
		},
	}
}

func newOrgTagCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "tag",
		Aliases: []string{"tags"},
		Short:   "Device tags that sends and routing rules target",
	}
	c.AddCommand(
		a.listCmd("list", "List device tags", "listOrgTags", 0,
			[]string{"ID", "NAME", "DEVICES", "DESCRIPTION"}, func(it map[string]any) []string {
				return []string{str(it, "id"), str(it, "name"), str(it, "device_count"), str(it, "description")}
			}),
		a.getCmd("get <tag-id>", "Show one tag", "getOrgTag"),
		a.writeCmd("create <name>", "Create a device tag (lowercase letters, digits, - and _)", "createOrgTag",
			[]field{describeField}, "created tag %s (%s)", "name", "id"),
		a.writeCmd("update <tag-id>", "Rename or describe a tag", "updateOrgTag",
			[]field{{"name", "name", kString, "new name"}, describeField}, "updated tag %s", "name"),
		a.deleteCmd("delete <tag-id>", "Delete a tag", "deleteOrgTag", "deleted", "tag"),
		newOrgTagDevicesCmd(a),
	)
	return group(c)
}

func newOrgTagDevicesCmd(a *App) *cobra.Command {
	var add, remove []string
	c := &cobra.Command{
		Use:   "devices <tag-id>",
		Short: "Tag and untag member devices",
		Long: `Add the tag to member devices or take it off. Each device is
<user-id>:<device-id>, as pushward org device list shows them.

  pushward org tag devices 7d3f... --add u1:DEVICE-A --add u2:DEVICE-B --remove u3:DEVICE-C`,
		Args:        checkArgs,
		Annotations: operation("updateOrgTagDevices"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			b := map[string]any{}
			for _, f := range []struct {
				flag string
				vals []string
			}{{"add", add}, {"remove", remove}} {
				if len(f.vals) == 0 {
					continue
				}
				list := make([]any, 0, len(f.vals))
				for _, v := range f.vals {
					user, device, ok := strings.Cut(v, ":")
					if !ok || user == "" || device == "" {
						return usagef("--%s %q: want <user-id>:<device-id>", f.flag, v)
					}
					list = append(list, map[string]any{"user_id": user, "device_id": device})
				}
				b[f.flag] = list
			}
			if len(b) == 0 {
				return usagef("nothing to change: give --add or --remove")
			}
			resp, err := a.call(cmd.Context(), "updateOrgTagDevices", map[string]string{"tag_id": argv[0]}, nil, b)
			if err != nil {
				return err
			}
			m := decode(resp.Body)
			return a.out().Printf(resp.Body, "tag %s is on %s devices", str(m, "name"), str(m, "device_count"))
		},
	}
	c.Flags().StringArrayVar(&add, "add", nil, "tag this device, as <user-id>:<device-id> (repeatable)")
	c.Flags().StringArrayVar(&remove, "remove", nil, "untag this device, as <user-id>:<device-id> (repeatable)")
	return c
}

var routeFields = []field{
	{"applies-to", "applies_to", kString, "what the rule routes: activities, notifications, both or widgets"},
	{"allow-group-ids", "allow_group_ids", kNames, "comma-separated ids of the groups whose members get what matches"},
	{"allow-tag-ids", "allow_tag_ids", kNames, "comma-separated ids of the device tags that get what matches"},
	{"note", "note", kString, "a note for the other admins; an empty value clears it on update"},
}

func newOrgRouteCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "route",
		Aliases: []string{"routes"},
		Short:   "Routing rules: who receives which slugs",
		Long: `Routing rules decide who receives an activity, notification or widget by
its slug. The first rule that matches decides; with no matching rule
everyone does. A new rule goes after the existing ones. On update,
-F allow_group_ids=[] or -F allow_tag_ids=[] empties a list.

  pushward org route create 'deploy-*' --applies-to both --allow-group-ids 3f1c...
  pushward org route reorder 3f1c... 9a2b...
  pushward org route apply`,
	}
	routeRow := func(it map[string]any) []string {
		return []string{str(it, "position"), str(it, "id"), str(it, "slug_pattern"), str(it, "applies_to"),
			idList(it, "allow_group_ids"), idList(it, "allow_tag_ids"), str(it, "note")}
	}
	routeHeaders := []string{"POS", "ID", "PATTERN", "APPLIES TO", "GROUPS", "TAGS", "NOTE"}
	c.AddCommand(
		a.listCmd("list", "List the rules in order", "listOrgRoutes", 0, routeHeaders, routeRow),
		a.getCmd("get <route-id>", "Show one rule", "getOrgRoute"),
		a.writeCmd("create <slug-pattern>", "Add a rule for a slug or a trailing-* pattern", "createOrgRoute",
			routeFields, "created route %s for %s (%s)", "slug_pattern", "applies_to", "id"),
		a.writeCmd("update <route-id>", "Change a rule", "updateOrgRoute",
			append([]field{{"slug-pattern", "slug_pattern", kString, "slug or trailing-* pattern the rule matches"}}, routeFields...),
			"updated route %s", "id"),
		a.deleteCmd("delete <route-id>", "Delete a rule", "deleteOrgRoute", "deleted", "route"),
		&cobra.Command{
			Use:         "reorder <route-id>...",
			Short:       "Put the rules in this order (every rule's id)",
			Args:        checkArgs,
			Annotations: operation("reorderOrgRoutes"),
			RunE: func(cmd *cobra.Command, argv []string) error {
				resp, err := a.call(cmd.Context(), "reorderOrgRoutes", nil, nil, map[string]any{"route_ids": argv})
				if err != nil {
					return err
				}
				return a.out().Items(resp.Body, routeHeaders, routeRow)
			},
		},
		&cobra.Command{
			Use:         "apply",
			Short:       "Re-apply the rules to what is already out",
			Args:        checkArgs,
			Annotations: operation("applyOrgRoutes"),
			RunE: func(cmd *cobra.Command, _ []string) error {
				if _, err := a.call(cmd.Context(), "applyOrgRoutes", nil, nil, nil); err != nil {
					return err
				}
				a.Status = "applied"
				a.out().Human("applied the routing rules")
				return nil
			},
		},
	)
	return group(c)
}

func newOrgDeviceCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "device",
		Aliases: []string{"devices"},
		Short:   "The members' devices and their tags",
	}
	c.AddCommand(
		a.listCmd("list", "List the members' devices", "listOrgDevices", 0,
			[]string{"USER ID", "MEMBER", "DEVICE ID", "NAME", "PLATFORM", "TAGS"}, func(it map[string]any) []string {
				return []string{str(it, "user_id"), str(it, "nickname"), str(it, "device_id"), str(it, "name"), str(it, "platform"),
					idList(it, "tag_ids")}
			}),
		newOrgSetCmd(a, "set-tags <user-id> <device-id> [<tag-id>...]", "Replace a member device's tags (none removes them all)", "setOrgDeviceTags", "tag_ids",
			"device %s has %d tags"),
	)
	return group(c)
}

func newOrgAuditCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "audit",
		Short: "The organization's audit log",
	}
	c.AddCommand(a.listCmd("list", "List audit events, newest first", "listOrgAuditEvents", 200,
		[]string{"TIME", "ACTOR", "ACTION", "TARGET"}, func(it map[string]any) []string {
			return []string{shortTime(str(it, "created_at")), kindID(str(it, "actor_kind"), cmp.Or(str(it, "actor_nickname"), str(it, "actor_user_id"))),
				str(it, "action"), kindID(str(it, "target_kind"), str(it, "target_id"))}
		}))
	return group(c)
}

// kindID renders an audit actor or target as kind:id, or the kind alone when
// the event names no one.
func kindID(kind, id string) string {
	if id == "" {
		return kind
	}
	return kind + ":" + id
}

func newOrgBillingCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "billing",
		Short: "The plan, seats and paid invoices (changes stay in the console)",
	}
	c.AddCommand(
		a.getCmd("get", "Show the plan, seats and subscription", "getOrgBilling"),
		a.listCmd("invoices", "List paid invoices", "listOrgInvoices", 0,
			[]string{"TRANSACTION ID", "NUMBER", "BILLED", "TOTAL", "STATUS"}, func(it map[string]any) []string {
				cents, _ := it["total_cents"].(float64)
				return []string{str(it, "transaction_id"), str(it, "invoice_number"), shortTime(str(it, "billed_at")),
					fmt.Sprintf("%.2f %s", cents/100, str(it, "currency")), str(it, "status")}
			}),
		newOrgInvoiceCmd(a),
	)
	return group(c)
}

func newOrgInvoiceCmd(a *App) *cobra.Command {
	var output string
	c := &cobra.Command{
		Use:   "invoice <transaction-id>",
		Short: "Print a link to an invoice PDF, or save the PDF with --output",
		Long: `Print a short-lived link to an invoice's PDF, or download the PDF to a file
with --output. The download goes to the link alone, without your key.

  pushward org billing invoice txn_01h... --output invoice.pdf`,
		Args:        checkArgs,
		Annotations: operation("getOrgInvoicePdf"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			resp, err := a.call(cmd.Context(), "getOrgInvoicePdf", map[string]string{"transaction_id": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			link := str(decode(resp.Body), "url")
			if output == "" {
				return a.out().Printf(resp.Body, "%s", link)
			}
			n, err := a.download(cmd.Context(), link, output)
			if err != nil {
				return err
			}
			a.out().Human("saved %s (%d bytes)", output, n)
			return nil
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "save the PDF to this file")
	return c
}

// maxDownloadBytes bounds an invoice PDF.
const maxDownloadBytes = 32 << 20

// downloadTimeout bounds the whole invoice download, which goes around the
// API client and its per-request timeout.
var downloadTimeout = 2 * time.Minute

// download saves what link points to into path. The link is checked like the
// API URL (https, or http on loopback) and gets no Authorization header.
func (a *App) download(ctx context.Context, link, path string) (int, error) {
	u, err := config.ValidateURL(link)
	if err != nil {
		return 0, fmt.Errorf("invoice link: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("downloading the invoice: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return 0, err
	}
	if len(data) > maxDownloadBytes {
		return 0, fmt.Errorf("the invoice is over %d bytes", maxDownloadBytes)
	}
	return len(data), os.WriteFile(path, data, 0o600)
}
