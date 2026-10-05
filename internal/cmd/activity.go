package cmd

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/body"
)

func newActivityCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "activity",
		Aliases: []string{"activities", "la"},
		Short:   "Create, update and end Live Activities",
		Long: `Live Activities are cards on the Lock Screen and in the Dynamic Island.

The usual flow is start, a few updates, then end:

  pushward activity start deploy --name "Deploy api" --template steps --step 1/3 --text Building
  pushward activity update deploy --step 2/3 --text Testing
  pushward activity end deploy --status success`,
	}
	c.AddCommand(
		newActivityListCmd(a),
		newActivityGetCmd(a),
		newActivityCreateCmd(a),
		newActivityUpdateCmd(a),
		newActivityDeleteCmd(a),
		newActivityStartCmd(a),
		newActivityEndCmd(a),
		newActivityWaitCmd(a),
	)
	return group(c)
}

func newActivityListCmd(a *App) *cobra.Command {
	var state string
	var pf pageFlags
	c := &cobra.Command{
		Use:         "list",
		Short:       "List activities",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "listActivities"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if state != "" {
				q.Set("state", state)
			}
			b, err := a.listPages(cmd, "listActivities", q, "after", pf)
			if err != nil {
				return err
			}
			return a.out().Items(b, []string{"SLUG", "STATE", "TEMPLATE", "NAME", "UPDATED"}, func(it map[string]any) []string {
				return []string{str(it, "slug"), str(it, "state"), str(it, "content", "template"), str(it, "name"), shortTime(str(it, "updated_at"))}
			})
		},
	}
	c.Flags().StringVar(&state, "state", "", "only this state: ongoing, ended, preempted")
	pf.register(c, "after")
	return c
}

// pageFlags are the paging flags the list endpoints share.
type pageFlags struct {
	limit  int
	cursor string
	all    bool
}

func (p *pageFlags) register(c *cobra.Command, cursorFlag string) {
	c.Flags().IntVar(&p.limit, "limit", 0, "page size, 1 to 100 (server default 50)")
	c.Flags().StringVar(&p.cursor, cursorFlag, "", "cursor from a previous page's next_cursor")
	c.Flags().BoolVar(&p.all, "all", false, "follow next_cursor and return every page")
}

// listPages fetches one page, or with --all every page, merged into a single
// {"items": [...]} body.
func (a *App) listPages(cmd *cobra.Command, opID string, q url.Values, cursorParam string, pf pageFlags) ([]byte, error) {
	limit := pf.limit
	if pf.all && limit == 0 {
		limit = 100 // the API's maximum: half the round trips of its default
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var items [][]byte
	cursor := pf.cursor
	for {
		if cursor != "" {
			q.Set(cursorParam, cursor)
		}
		resp, err := a.call(cmd.Context(), opID, nil, q, nil)
		if err != nil {
			return nil, err
		}
		if !pf.all {
			return resp.Body, nil
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			return nil, fmt.Errorf("decoding page: %w", err)
		}
		for _, it := range page.Items {
			items = append(items, it)
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	merged := slices.Concat([]byte(`{"items":[`), bytes.Join(items, []byte(",")), []byte(`]}`))
	a.Body = merged
	return merged, nil
}

func newActivityGetCmd(a *App) *cobra.Command {
	var include string
	var wait time.Duration
	c := &cobra.Command{
		Use:         "get <slug>",
		Short:       "Show one activity",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "getActivity"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			q := url.Values{}
			if include != "" {
				q.Set("include", include)
			}
			if wait >= time.Second {
				q.Set("wait", strconv.Itoa(int(min(wait, maxServerWait)/time.Second)))
			}
			resp, err := a.call(cmd.Context(), "getActivity", map[string]string{"slug": argv[0]}, q, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, nil)
		},
	}
	c.Flags().StringVar(&include, "include", "", "extras to include: log_backlog")
	c.Flags().DurationVar(&wait, "wait", 0, "approval template: hold the request up to this long (max 25s) for an answer")
	return c
}

func newActivityCreateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var name string
	c := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create an activity (it shows on devices after its first update)",
		Long: `Create an activity. Creating a slug that already exists refreshes its
name, priority and TTLs instead of failing. With --target-* it also replaces
the stored target at once (devices that lose it end it, devices that gain it
start it); without them the stored target stays.

A new activity has no content: nothing shows on a device until an update
sets a template. "activity start" does both in one step.`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "createActivity"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			conv, err := collectFields(cmd, slices.Concat(activityLifecycleFields, targetFields), a.Now())
			if err != nil {
				return err
			}
			conv["slug"] = slug
			conv["name"] = cmp.Or(name, slug)
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "createActivity", nil, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "created activity %s", slug)
		},
	}
	c.Flags().StringVar(&name, "name", "", "display name (default: the slug)")
	addFields(c, activityLifecycleFields)
	addFields(c, targetFields)
	bf.register(c)
	return c
}

func newActivityUpdateCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var upsert, noTarget bool
	var state, sound string
	c := &cobra.Command{
		Use:   "update <slug>",
		Short: "Update an activity's content or state",
		Long: `Update an activity. The body is a JSON merge patch: fields you leave out
keep their stored value, and null clears one.

  pushward activity update build --progress 0.6 --text "Linking"
  pushward activity update build -F content.progress=0.6 -f content.state=Linking`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "updateActivity"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			conv, err := collectFields(cmd, slices.Concat(activityContentFields, activityLifecycleFields, targetFields), a.Now())
			if err != nil {
				return err
			}
			if noTarget {
				if _, set := conv["target"]; set {
					return usagef("--no-target cannot be used with --target-groups, --target-tags or --target-members")
				}
				conv["target"] = nil
			}
			if err := setLifecycleState(conv, state); err != nil {
				return err
			}
			if sound != "" {
				conv["sound"] = sound
			}
			if err := addOptions(cmd, conv); err != nil {
				return err
			}
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 {
				return usagef("nothing to update: set a flag, -f/-F or --data")
			}
			q := url.Values{}
			if upsert {
				q.Set("upsert", "true")
			}
			resp, err := a.call(cmd.Context(), "updateActivity", map[string]string{"slug": slug}, q, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "updated activity %s (%s)", slug, str(decode(resp.Body), "state"))
		},
	}
	c.Flags().BoolVar(&upsert, "upsert", false, "create the activity first if it does not exist (needs activity:manage)")
	c.Flags().StringVar(&state, "state", "", "lifecycle state: ongoing or ended")
	c.Flags().StringVar(&sound, "sound", "", "alert sound: default, chime, alert, success, warning, bell, ding, buzz, notification")
	c.Flags().BoolVar(&noTarget, "no-target", false, "organization keys: clear the target, so everyone the routing rules allow gets it")
	addFields(c, activityContentFields)
	addFields(c, activityLifecycleFields)
	addFields(c, targetFields)
	addOptionFlag(c)
	bf.register(c)
	return c
}

func setLifecycleState(conv map[string]any, state string) error {
	switch state {
	case "":
	case "ongoing", "ended", "ONGOING", "ENDED":
		conv["state"] = state
	default:
		return usagef("--state: want ongoing or ended, got %q", state)
	}
	return nil
}

// addOptionFlag adds --option, the approval template's answer buttons.
func addOptionFlag(c *cobra.Command) {
	c.Flags().StringArray("option", nil, "approval template: answer button as id=Title (repeatable, 2 to 4)")
}

func addOptions(cmd *cobra.Command, conv map[string]any) error {
	vals, _ := cmd.Flags().GetStringArray("option")
	if len(vals) == 0 {
		return nil
	}
	opts, err := buttons("option", vals)
	if err != nil {
		return err
	}
	return body.Set(conv, "content.options", opts)
}

func newActivityDeleteCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <slug>",
		Short:       "Delete an activity and remove it from devices",
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "deleteActivity"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			if _, err := a.call(cmd.Context(), "deleteActivity", map[string]string{"slug": argv[0]}, nil, nil); err != nil {
				return err
			}
			a.out().Human("deleted activity %s", argv[0])
			return nil
		},
	}
}

func newActivityStartCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var name string
	c := &cobra.Command{
		Use:   "start <slug>",
		Short: "Create an activity and show it (create, then update to ongoing)",
		Long: `Create an activity if needed and set it ongoing with its first content.
Starting a slug that already exists restarts it with the new content.

--template defaults to generic. -f, -F and --data apply to the update body.
--target-* go with the create, so a restart can change the target; a target
in -f, -F or --data goes with the update instead, so use one or the other.

  pushward activity start backup --name "Nightly backup" --text "Copying" --progress 0`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			create, err := collectFields(cmd, slices.Concat(activityLifecycleFields, targetFields), a.Now())
			if err != nil {
				return err
			}
			patch, err := collectFields(cmd, activityContentFields, a.Now())
			if err != nil {
				return err
			}
			if err := addOptions(cmd, patch); err != nil {
				return err
			}
			patch["state"] = "ongoing"
			b, err := bf.build(a, patch)
			if err != nil {
				return err
			}
			if _, ok := b["target"]; ok && create["target"] != nil {
				return usagef("use --target-* or a target in -f, -F or --data, not both")
			}
			if str(b, "content", "template") == "" {
				if err := body.Set(b, "content.template", "generic"); err != nil {
					return usagef("%v", err)
				}
			}
			create["slug"] = slug
			create["name"] = cmp.Or(name, slug)
			if _, err := a.call(cmd.Context(), "createActivity", nil, nil, create); err != nil {
				return err
			}
			resp, err := a.call(cmd.Context(), "updateActivity", map[string]string{"slug": slug}, nil, b)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "started activity %s", slug)
		},
	}
	c.Flags().StringVar(&name, "name", "", "display name (default: the slug)")
	addFields(c, activityContentFields)
	addFields(c, activityLifecycleFields)
	addFields(c, targetFields)
	addOptionFlag(c)
	bf.register(c)
	return c
}

// endStyle is how a finished activity looks for each job outcome. The texts
// match the pushward-github bridge, so a card reads the same whichever of
// the two produced it.
var endStyle = map[string]struct{ text, icon, color string }{
	"success":   {"Success", "checkmark.circle.fill", "green"},
	"failure":   {"Failed", "xmark.circle.fill", "red"},
	"cancelled": {"Cancelled", "stop.circle.fill", "#8E8E93"},
}

var dismissField = []field{{"dismiss-after", "dismissal_ttl", kSecondsZero, "remove from the Lock Screen this long after ending (max 4h)"}}

func newActivityEndCmd(a *App) *cobra.Command {
	var bf bodyFlags
	var status, text, icon, color string
	var displayTime time.Duration
	var ignoreMissing bool
	c := &cobra.Command{
		Use:   "end <slug>",
		Short: "End an activity, showing a final state first",
		Long: `End an activity. When the outcome changes the card, it first shows the
final look and holds it for --display-time, then ends, so the last frame on
the Lock Screen is the outcome rather than whatever the previous update
showed.

--status success|failure|cancelled sets the text, icon and color (the values
of GitHub's job.status). --text, --icon and --color override them. Approval
cards keep their question and icon.`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			if _, ok := endStyle[status]; status != "" && !ok {
				return usagef("--status: want success, failure or cancelled, got %q", status)
			}
			final, err := collectFields(cmd, dismissField, a.Now())
			if err != nil {
				return err
			}
			final["state"] = "ended"
			extra, err := bf.build(a, nil)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			params := map[string]string{"slug": slug}
			resp, err := a.call(ctx, "getActivity", params, nil, nil)
			var ae *api.Error
			switch {
			case ignoreMissing && errors.As(err, &ae) && ae.Status == http.StatusNotFound:
				a.out().Warnf("activity %s does not exist; nothing to end", slug)
				return nil
			case err != nil:
				return err
			}
			current := decode(resp.Body)
			if str(current, "state") == "ended" {
				return a.out().Printf(resp.Body, "activity %s had already ended", slug)
			}

			stored, _ := current["content"].(map[string]any)
			first := map[string]any{}
			if delta := endDelta(stored, status); delta != nil {
				for k, v := range map[string]string{"state": text, "icon": icon, "accent_color": color} {
					if v != "" {
						delta[k] = v
					}
				}
				first["content"] = delta
			}
			body.Merge(first, extra)

			if displayTime > 0 && frameChanges(first, stored) {
				first["state"] = "ongoing"
				if _, err := a.call(ctx, "updateActivity", params, nil, first); err != nil {
					return err
				}
				if err := a.sleep(ctx, displayTime); err != nil {
					return err
				}
			} else {
				// One patch does it all. Content that is only the template
				// changes nothing, so leave it out.
				if c, _ := first["content"].(map[string]any); len(c) == 1 && c["template"] != nil {
					delete(first, "content")
				}
				maps.Copy(first, final)
				final = first
			}
			resp, err = a.call(ctx, "updateActivity", params, nil, final)
			if err != nil {
				return err
			}
			return a.out().Printf(resp.Body, "ended activity %s", slug)
		},
	}
	c.Flags().StringVar(&status, "status", "", "outcome: success, failure or cancelled")
	c.Flags().StringVar(&text, "text", "", "final status text")
	c.Flags().StringVar(&icon, "icon", "", "final icon")
	c.Flags().StringVar(&color, "color", "", "final accent color")
	c.Flags().DurationVar(&displayTime, "display-time", 4*time.Second, "how long to show the final state before ending (0 ends at once)")
	c.Flags().BoolVar(&ignoreMissing, "ignore-missing", false, "exit 0 when the activity does not exist")
	addFields(c, dismissField)
	bf.register(c)
	return c
}

// endDelta builds the content patch for the final frame: the template plus
// the fields the outcome sets. Nil means the activity has no content yet, so
// there is no frame to show and it only needs ending.
func endDelta(stored map[string]any, status string) map[string]any {
	tmpl, _ := stored["template"].(string)
	if tmpl == "" {
		return nil
	}
	delta := map[string]any{"template": tmpl}
	if live, _ := stored["live_progress"].(bool); live {
		// Stop the device animating the bar past the end.
		delta["live_progress"] = nil
		delta["remaining_time"] = nil
	}
	if tmpl == "approval" {
		// The question lives in state and the icon is the card's identity,
		// so only explicit overrides touch them.
		return delta
	}
	style, ok := endStyle[status]
	if !ok {
		if s, _ := stored["state"].(string); s == "" {
			delta["state"] = "Ended"
		}
		return delta
	}
	delta["state"] = style.text
	delta["icon"] = style.icon
	delta["accent_color"] = style.color
	if status == "success" && (tmpl == "generic" || tmpl == "steps") {
		delta["progress"] = 1
		if total, ok := stored["total_steps"].(float64); ok && tmpl == "steps" && total > 0 {
			delta["current_step"] = int(total)
		}
	}
	return delta
}

// frameChanges reports whether the final-frame patch changes what the card
// shows. The template only rides along so the merge patch validates, and
// stopping the progress animation can wait for the end push, so neither is
// worth a second update on its own.
func frameChanges(patch, stored map[string]any) bool {
	for k, v := range patch {
		c, ok := v.(map[string]any)
		if k != "content" || !ok {
			return true
		}
		for ck, cv := range c {
			switch ck {
			case "template", "live_progress", "remaining_time":
				continue
			}
			if !sameJSON(cv, stored[ck]) {
				return true
			}
		}
	}
	return false
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func newActivityWaitCmd(a *App) *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "wait <slug>",
		Short: "Wait for the answer to an approval activity",
		Long: `Wait until someone answers an approval activity, it ends, or --timeout
passes. Prints the activity; the chosen option is content.answer.option.
Exits 7 on timeout.

  pushward activity start release --template approval --text "Ship 2.4?" \
    --option ship=Ship --option hold=Hold
  pushward activity wait release --timeout 30m --jq .content.answer.option`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			slug := argv[0]
			var option string
			last, err := a.poll(cmd.Context(), "getActivity", map[string]string{"slug": slug}, timeout, func(b []byte) (bool, error) {
				act := decode(b)
				if tmpl := str(act, "content", "template"); tmpl != "approval" {
					return false, fmt.Errorf("activity %s is template %q, not approval", slug, tmpl)
				}
				option = str(act, "content", "answer", "option")
				if option == "" && str(act, "state") == "ended" {
					return false, errors.New("the activity ended with no recorded answer")
				}
				return option != "", nil
			})
			switch {
			case err == nil:
				return a.out().Printf(last, "answered: %s", option)
			case last != nil:
				_ = a.out().Print(last, nil)
			}
			return err
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "give up after this long")
	return c
}
