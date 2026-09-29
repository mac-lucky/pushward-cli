package cmd

import (
	"fmt"
	"io"
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
creates cannot have a higher scope or a capability the default key lacks,
and keeps working if the default key is later revoked or rolled.

  pushward key create backup --notifications --activity-slugs 'backup-*' --jq .key
  pushward key list
  pushward key revoke 0b6f0c1e-5a3d-4c1f-9d0a-6f2e8b7c4a11`,
	}
	c.AddCommand(newKeyListCmd(a), newKeyCreateCmd(a), newKeyUpdateCmd(a), newKeyRollCmd(a), newKeyRevokeCmd(a))
	return group(c)
}

var keyFields = []field{
	{"scope", "scope", kString, "activity:update (list, read and update activities) or activity:manage (also create and delete them)"},
	{"activity-slugs", "activity_slugs", kCSV, "comma-separated activity slugs or trailing-* patterns the key is limited to"},
}

// keyCapabilities are bool flags rather than fields, so --notifications=false
// can switch one off.
var keyCapabilities = []struct{ flag, usage string }{
	{"notifications", "allow sending notifications"},
	{"widgets", "allow managing widgets"},
	{"emails", "allow sending transactional emails"},
}

func addKeyFlags(c *cobra.Command) {
	addFields(c, keyFields)
	for _, k := range keyCapabilities {
		c.Flags().Bool(k.flag, false, k.usage)
	}
}

// collectKeyFields returns the body fragment for the key flags the user set.
func collectKeyFields(cmd *cobra.Command, a *App) (map[string]any, error) {
	conv, err := collectFields(cmd, keyFields, a.Now())
	if err != nil {
		return nil, err
	}
	for _, k := range keyCapabilities {
		if cmd.Flags().Changed(k.flag) {
			conv[k.flag], _ = cmd.Flags().GetBool(k.flag)
		}
	}
	return conv, nil
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
			return a.out().Items(resp.Body, []string{"ID", "NAME", "SCOPE", "CAPABILITIES", "ACTIVITIES", "DEFAULT", "LAST USED"}, func(it map[string]any) []string {
				var caps []string
				for _, k := range keyCapabilities {
					if on, _ := it[k.flag].(bool); on {
						caps = append(caps, k.flag)
					}
				}
				activities := "all"
				if slugs, _ := it["activity_slugs"].([]any); len(slugs) > 0 {
					list := make([]string, 0, len(slugs))
					for _, s := range slugs {
						list = append(list, fmt.Sprint(s))
					}
					activities = strings.Join(list, ",")
				}
				def := ""
				if d, _ := it["is_default"].(bool); d {
					def = "yes"
				}
				return []string{str(it, "id"), str(it, "name"), str(it, "scope"), strings.Join(caps, ","), activities, def, shortTime(str(it, "last_used_at"))}
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
--jq .key prints just the secret. Without flags the key gets
activity:update on every activity and no capabilities.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "createIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if err := a.errQuietSecret(); err != nil {
				return err
			}
			conv, err := collectKeyFields(cmd, a)
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
	var allActivities bool
	c := &cobra.Command{
		Use:   "update <key-id>",
		Short: "Change an integration key's scope, activities or capabilities",
		Long: `Change a key's scope, activity restriction or capabilities. Flags you leave
out keep their stored value, and --all-activities removes the activity
restriction. The default key cannot be changed here.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "updateIntegrationKey"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			conv, err := collectKeyFields(cmd, a)
			if err != nil {
				return err
			}
			if allActivities {
				if conv["activity_slugs"] != nil {
					return usagef("--all-activities and --activity-slugs cannot be used together")
				}
				conv["activity_slugs"] = []any{}
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
