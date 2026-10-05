package cmd

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newKeyCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "key",
		Aliases: []string{"keys"},
		Short:   "Create, change and revoke integration keys",
		Long: `Manage the account's integration keys. Only the account's default key can
do this; any other key gets integration_key.default_key_required. A key it
creates cannot have a permission above the default key's own, and keeps
working if the default key is later revoked or rolled.

Each key has a level per resource: --activities none|read|update|manage,
--notifications=none|send|schedule, --widgets=none|read|write and
--emails=none|send (those three take their level after =). On their own, a
bare flag or =true keeps its old meaning (the highest level; for
--notifications, send when the default key itself can only send) and =false
means none; next to another level flag each needs a level of its own. A key
can also be limited to activity and widget slugs and given an expiry.

  pushward key create backup --notifications=send --activity-slugs 'backup-*' --jq .key
  pushward key create alerts --activities none --notifications=send
  pushward key create dashboard --activities read --widgets=read --expires 90d
  pushward key list
  pushward key revoke 0b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11`,
	}
	c.AddCommand(newKeyListCmd(a), newKeyCreateCmd(a), newKeyUpdateCmd(a), newKeyRollCmd(a), newKeyRevokeCmd(a))
	return group(c)
}

var keyFields = []field{
	{"scope", "scope", kString, "legacy form of --activities: activity:none, activity:read, activity:update or activity:manage"},
	{"activity-slugs", "activity_slugs", kNames, "comma-separated activity slugs or trailing-* patterns the key is limited to"},
	{"widget-slugs", "widget_slugs", kNames, "comma-separated widget slugs or trailing-* patterns the key is limited to"},
	{"expires", "expires_at", kRFC3339, "when the key stops working: an RFC 3339 time (2026-12-31T00:00:00Z), unix seconds, or a duration from now (90d, 12h)"},
}

// keyLevels are the permission level flags. notifications, widgets and emails
// were booleans before levels existed and still take true/false on their own,
// sent as the legacy fields so a bare --notifications keeps the meaning the
// server gives it.
var keyLevels = []struct {
	flag   string
	levels []string
	legacy bool
	usage  string
}{
	{"activities", []string{"none", "read", "update", "manage"}, false, "activity level: none, read (list and get), update (also PATCH) or manage (also create and delete)"},
	{"notifications", []string{"none", "send", "schedule"}, true, "notification level: none, send, or schedule (also scheduled notifications); a bare --notifications on its own is the legacy true"},
	{"widgets", []string{"none", "read", "write"}, true, "widget level: none, read, or write (also create, update and delete); a bare --widgets on its own means write"},
	{"emails", []string{"none", "send"}, true, "email level: none or send; a bare --emails on its own means send"},
}

// keyArgs is checkArgs with a hint for the one mistake the optional-value
// flags invite: --notifications send parses as a bare --notifications (true)
// plus a stray argument, so the level has to be joined with =. A level word
// among the arguments while such a flag is bare is refused even when the
// count fits, since it may have been taken as the key's name.
func keyArgs(cmd *cobra.Command, got []string) error {
	for _, k := range keyLevels {
		if !k.legacy || !cmd.Flags().Changed(k.flag) {
			continue
		}
		if v, _ := cmd.Flags().GetString(k.flag); v != "true" {
			continue
		}
		for _, arg := range got {
			if slices.Contains(k.levels, arg) {
				return usagef("write --%s=%s: a level after --%s needs = (for a key named %q, give --%s=%s explicitly)",
					k.flag, arg, k.flag, arg, k.flag, k.levels[len(k.levels)-1])
			}
		}
	}
	return checkArgs(cmd, got)
}

func addKeyFlags(c *cobra.Command) {
	addFields(c, keyFields)
	for _, k := range keyLevels {
		c.Flags().String(k.flag, "", k.usage)
		if k.legacy {
			c.Flags().Lookup(k.flag).NoOptDefVal = "true"
		}
	}
}

// collectKeyFields returns the body fragment for the key flags the user set.
// The API takes either a permissions object or the legacy scope and
// true/false fields, never both, so once any flag names a level everything
// goes into permissions; with true/false alone the legacy fields are sent as
// before. A true next to a level is refused rather than translated: what a
// legacy true grants is the server's call (for notifications it depends on the
// calling key), and one rule for all three flags is easier to state.
// On create the permissions object gives a resource left out none, while the
// legacy default is update for activities, so create fills that in to keep
// --widgets=write and --widgets the same key.
func collectKeyFields(cmd *cobra.Command, a *App, create bool) (map[string]any, error) {
	conv, err := collectFields(cmd, keyFields, a.Now())
	if err != nil {
		return nil, err
	}
	scope, hasScope := conv["scope"].(string)
	if hasScope && !slices.Contains(keyScopes, scope) {
		return nil, usagef("--scope: want one of %s, got %q", strings.Join(keyScopes, ", "), scope)
	}
	levels := map[string]any{}
	legacy := map[string]bool{}
	for _, k := range keyLevels {
		if !cmd.Flags().Changed(k.flag) {
			continue
		}
		v, _ := cmd.Flags().GetString(k.flag)
		if slices.Contains(k.levels, v) {
			levels[k.flag] = v
			continue
		}
		on, err := strconv.ParseBool(v)
		if !k.legacy || err != nil {
			return nil, usagef("--%s: want one of %s, got %q", k.flag, strings.Join(k.levels, ", "), v)
		}
		legacy[k.flag] = on
	}
	if _, ok := levels["activities"]; ok && hasScope {
		return nil, usagef("--activities and --scope cannot be used together")
	}
	if len(levels) == 0 {
		for flag, on := range legacy {
			conv[flag] = on
		}
		return conv, nil
	}
	for _, k := range keyLevels {
		on, ok := legacy[k.flag]
		if !ok {
			continue
		}
		if on {
			return nil, usagef("--%s: give a level (--%s=%s) when another flag sets a level", k.flag, k.flag, strings.Join(k.levels[1:], " or --"+k.flag+"="))
		}
		levels[k.flag] = k.levels[0]
	}
	if hasScope {
		levels["activities"] = strings.TrimPrefix(scope, "activity:")
		delete(conv, "scope")
	}
	if _, ok := levels["activities"]; !ok && create {
		levels["activities"] = "update"
	}
	conv["permissions"] = levels
	return conv, nil
}

// keyScopes are the values --scope takes.
var keyScopes = []string{"activity:none", "activity:read", "activity:update", "activity:manage"}

// keyPermissions renders a key's levels for the list table, leaving out the
// resources it cannot use. Responses from a server without levels fall back
// to the legacy fields.
func keyPermissions(it map[string]any) string {
	var out []string
	if p, ok := it["permissions"].(map[string]any); ok {
		for _, k := range keyLevels {
			if v := str(p, k.flag); v != "" && v != "none" {
				out = append(out, k.flag+":"+v)
			}
		}
	} else {
		out = append(out, str(it, "scope"))
		for _, k := range keyLevels[1:] {
			if on, _ := it[k.flag].(bool); on {
				out = append(out, k.flag)
			}
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, " ")
}

// slugList renders a slug restriction, "all" when there is none.
func slugList(it map[string]any, key string) string {
	slugs, _ := it[key].([]any)
	if len(slugs) == 0 {
		return "all"
	}
	list := make([]string, 0, len(slugs))
	for _, s := range slugs {
		list = append(list, fmt.Sprint(s))
	}
	return strings.Join(list, ",")
}

// errQuietSecret refuses -q on the commands whose response holds a secret
// that is shown only once.
func (a *App) errQuietSecret() error {
	if a.quiet {
		return usagef("--quiet would discard the new key, which is shown only once; use --jq .key to print just the key")
	}
	return nil
}

func newKeyListCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List integration keys",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "listIntegrationKeys"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := a.call(cmd.Context(), "listIntegrationKeys", nil, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Items(resp.Body, []string{"ID", "NAME", "PERMISSIONS", "ACTIVITIES", "WIDGETS", "EXPIRES", "DEFAULT", "LAST USED"}, func(it map[string]any) []string {
				def := ""
				if d, _ := it["is_default"].(bool); d {
					def = "yes"
				}
				return []string{
					str(it, "id"), str(it, "name"), keyPermissions(it),
					slugList(it, "activity_slugs"), slugList(it, "widget_slugs"),
					shortTime(str(it, "expires_at")), def, shortTime(str(it, "last_used_at")),
				}
			})
		},
	}
}

func newKeyCreateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an integration key",
		Long: `Create an integration key and print its secret, which is shown only once;
--jq .key prints just the secret. Without flags the key can read and update
every activity and nothing else. Once any level flag is given, a resource
you leave out gets none, except activities, which stay at update unless you
pass --activities (--activities none for a key that only sends
notifications).`,
		Args:        keyArgs,
		Annotations: map[string]string{"operation": "createIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if err := a.errQuietSecret(); err != nil {
				return err
			}
			conv, err := collectKeyFields(cmd, a, true)
			if err != nil {
				return err
			}
			conv["name"] = argv[0]
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createIntegrationKey", nil, nil, b)
			if err != nil {
				return err
			}
			return a.printKeySecret(resp.Body, "created")
		},
	}
	addKeyFlags(c)
	bf.register(c)
	return c
}

func newKeyUpdateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var allActivities, allWidgets, noExpiry bool
	c := &cobra.Command{
		Use:   "update <key-id>",
		Short: "Change an integration key's permissions, slugs or expiry",
		Long: `Change a key's permission levels, slug restrictions or expiry. Flags you
leave out keep their stored value; --all-activities and --all-widgets remove
a slug restriction and --no-expiry removes the expiry. The default key's
activity level, slugs and expiry cannot be changed.`,
		Args:        keyArgs,
		Annotations: map[string]string{"operation": "updateIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectKeyFields(cmd, a, false)
			if err != nil {
				return err
			}
			for _, clear := range []struct {
				on         bool
				flag, with string
				key        string
				value      any
			}{
				{allActivities, "all-activities", "activity-slugs", "activity_slugs", []any{}},
				{allWidgets, "all-widgets", "widget-slugs", "widget_slugs", []any{}},
				{noExpiry, "no-expiry", "expires", "expires_at", nil},
			} {
				if !clear.on {
					continue
				}
				if _, set := conv[clear.key]; set {
					return usagef("--%s and --%s cannot be used together", clear.flag, clear.with)
				}
				conv[clear.key] = clear.value
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 {
				return usagef("nothing to update: set a flag, -f/-F or --data")
			}
			resp, err := a.call(cmd.Context(), "updateIntegrationKey", map[string]string{"key_id": argv[0]}, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "updated key %s", argv[0])
		},
	}
	addKeyFlags(c)
	c.Flags().BoolVar(&allActivities, "all-activities", false, "remove the activity restriction")
	c.Flags().BoolVar(&allWidgets, "all-widgets", false, "remove the widget restriction")
	c.Flags().BoolVar(&noExpiry, "no-expiry", false, "remove the expiry")
	bf.register(c)
	return c
}

func newKeyRollCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "roll <key-id>",
		Short: "Replace an integration key's secret",
		Long: `Replace a key's secret and print the new one, which is shown only once.
The old secret stops working at once. The default key cannot be rolled here.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "rollIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if err := a.errQuietSecret(); err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "rollIntegrationKey", map[string]string{"key_id": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			return a.printKeySecret(resp.Body, "rolled")
		},
	}
}

func newKeyRevokeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an integration key",
		Long: `Revoke a key: requests made with it fail from then on, and its pending
scheduled notifications are dropped. The default key cannot be revoked here.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "revokeIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if _, err := a.call(cmd.Context(), "revokeIntegrationKey", map[string]string{"key_id": argv[0]}, nil, nil); err != nil {
				return err
			}
			// A 204 has no body to take the status from.
			a.Status = "revoked"
			a.out().Human("revoked key %s", argv[0])
			return nil
		},
	}
}

// printKeySecret prints a created or rolled key. On a terminal the secret
// gets its own line, since this is the only time it is shown.
func (a *App) printKeySecret(body []byte, verb string) error {
	m := decode(body)
	return a.out().Print(body, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s key %s (%s)\n%s\nStore it now: the key is not shown again.\n", verb, str(m, "name"), str(m, "id"), str(m, "key"))
		return err
	})
}
