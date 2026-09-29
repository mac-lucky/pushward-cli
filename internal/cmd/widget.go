package cmd

import (
	"slices"

	"github.com/spf13/cobra"
)

func newWidgetCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "widget",
		Aliases: []string{"widgets"},
		Short:   "Create and update home screen widgets",
		Long: `Widgets are home screen and Lock Screen widgets fed by the API.

  pushward widget create coverage --name Coverage --template progress --value 0.81 --label api
  pushward widget update coverage --value 0.84`,
	}
	c.AddCommand(newWidgetListCmd(a), newWidgetGetCmd(a), newWidgetCreateCmd(a), newWidgetUpdateCmd(a), newWidgetDeleteCmd(a))
	return group(c)
}

func newWidgetListCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List widgets",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "listWidgets"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := a.call(cmd.Context(), "listWidgets", nil, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Items(resp.Body, []string{"SLUG", "TEMPLATE", "NAME", "VALUE", "UPDATED"}, func(it map[string]any) []string {
				return []string{str(it, "slug"), str(it, "content", "template"), str(it, "name"), str(it, "content", "value"), shortTime(str(it, "updated_at"))}
			})
		},
	}
}

func newWidgetGetCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "get <slug>",
		Short:       "Show one widget",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getWidget"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			resp, err := a.call(cmd.Context(), "getWidget", map[string]string{"slug": argv[0]}, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, nil)
		},
	}
}

func newWidgetCreateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:         "create <slug>",
		Short:       "Create a widget",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "createWidget"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			conv, err := collectFields(cmd, slices.Concat(widgetMetaFields, widgetContentFields), a.Now())
			if err != nil {
				return err
			}
			conv["slug"] = slug
			if conv["name"] == nil {
				conv["name"] = slug
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createWidget", nil, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "created widget %s", slug)
		},
	}
	addFields(c, widgetMetaFields)
	addFields(c, widgetContentFields)
	bf.register(c)
	return c
}

func newWidgetUpdateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:   "update <slug>",
		Short: "Update a widget",
		Long: `Update a widget with a JSON merge patch: content fields you leave out keep
their stored value, and null clears one.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "updateWidget"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			conv, err := collectFields(cmd, slices.Concat(widgetMetaFields, widgetContentFields), a.Now())
			if err != nil {
				return err
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 {
				return usagef("nothing to update: set a flag, -f/-F or --data")
			}
			// The API requires content on every update, even an empty one.
			if b["content"] == nil {
				b["content"] = map[string]any{}
			}
			resp, err := a.call(cmd.Context(), "updateWidget", map[string]string{"slug": slug}, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "updated widget %s", slug)
		},
	}
	addFields(c, widgetMetaFields)
	addFields(c, widgetContentFields)
	bf.register(c)
	return c
}

func newWidgetDeleteCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <slug>",
		Short:       "Delete a widget",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "deleteWidget"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if _, err := a.call(cmd.Context(), "deleteWidget", map[string]string{"slug": argv[0]}, nil, nil); err != nil {
				return err
			}
			a.out().Human("deleted widget %s", argv[0])
			return nil
		},
	}
}
