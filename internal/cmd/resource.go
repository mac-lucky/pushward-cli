package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

// The org, device, share and recipient commands are mostly the same few
// shapes: list a collection, get or delete one item, send a small body. These
// helpers build them; a positional fills the path parameter of its name
// (<group-id> is {group_id}, and an <x-id> fills a bare {id}), and any other
// positional goes in the body under its own name (<name> is "name").

func splitArgs(cmd *cobra.Command, opID string, argv []string) (params map[string]string, body map[string]any) {
	path := api.Op(opID).Path
	params, body = map[string]string{}, map[string]any{}
	for i, name := range positionals(cmd) {
		k := strings.ReplaceAll(name, "-", "_")
		switch {
		case strings.Contains(path, "{"+k+"}"):
			params[k] = argv[i]
		case strings.HasSuffix(k, "_id") && strings.Contains(path, "{id}"):
			params["id"] = argv[i]
		default:
			body[k] = argv[i]
		}
	}
	return params, body
}

func operation(opID string) map[string]string {
	return map[string]string{"operation": opID}
}

// listCmd prints a collection as a table. With pageMax set the operation is
// paged: every page is fetched, up to --limit items.
func (a *App) listCmd(use, short, opID string, pageMax int, headers []string, row func(map[string]any) []string) *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			params, _ := splitArgs(cmd, opID, argv)
			var b []byte
			if pageMax > 0 {
				if limit < 0 {
					return usagef("--limit must be at least 1")
				}
				var err error
				if b, err = a.allPages(cmd.Context(), opID, params, limit, pageMax); err != nil {
					return err
				}
			} else {
				resp, err := a.call(cmd.Context(), opID, params, nil, nil)
				if err != nil {
					return err
				}
				b = resp.Body
			}
			return a.out().Items(b, headers, row)
		},
	}
	if pageMax > 0 {
		c.Flags().IntVar(&limit, "limit", 0, "stop after this many items (default: all of them)")
	}
	return c
}

// allPages follows next_cursor to the last page, or until limit items when
// limit is set, and returns the items as one {"items": [...]} body.
func (a *App) allPages(ctx context.Context, opID string, params map[string]string, limit, pageMax int) ([]byte, error) {
	items := []json.RawMessage{}
	q := url.Values{}
	for {
		size := pageMax
		if limit > 0 {
			size = min(limit-len(items), pageMax)
		}
		q.Set("limit", strconv.Itoa(size))
		resp, err := a.call(ctx, opID, params, q, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			return nil, fmt.Errorf("decoding page: %w", err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" || page.NextCursor == q.Get("cursor") || (limit > 0 && len(items) >= limit) {
			break
		}
		q.Set("cursor", page.NextCursor)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	merged, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return nil, err
	}
	a.Body = merged
	return merged, nil
}

// getCmd prints one item as JSON.
func (a *App) getCmd(use, short, opID string) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			params, _ := splitArgs(cmd, opID, argv)
			resp, err := a.call(cmd.Context(), opID, params, nil, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, nil)
		},
	}
}

// deleteCmd removes one item, with no prompt: scripts run these.
func (a *App) deleteCmd(use, short, opID, verb, noun string) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			params, _ := splitArgs(cmd, opID, argv)
			if _, err := a.call(cmd.Context(), opID, params, nil, nil); err != nil {
				return err
			}
			// A 204 has no body to take the status from.
			a.Status = verb
			a.out().Human("%s %s %s", verb, noun, strings.Join(argv, " "))
			return nil
		},
	}
}

// writeCmd sends the body its flags build and prints done, filled with the
// response fields keys name. A PATCH with nothing to send is refused.
func (a *App) writeCmd(use, short, opID string, fields []field, done string, keys ...string) *cobra.Command {
	var bf bodyFlags
	c := &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        checkArgs,
		Annotations: operation(opID),
		RunE: func(cmd *cobra.Command, argv []string) error {
			params, conv := splitArgs(cmd, opID, argv)
			flags, err := collectFields(cmd, fields, a.Now())
			if err != nil {
				return err
			}
			maps.Copy(conv, flags)
			b, err := bf.build(a, conv)
			if err != nil {
				return err
			}
			if len(b) == 0 && api.Op(opID).Method == http.MethodPatch {
				return usagef("nothing to update: set a flag, -f/-F or --data")
			}
			resp, err := a.call(cmd.Context(), opID, params, nil, b)
			if err != nil {
				return err
			}
			m := decode(resp.Body)
			vals := make([]any, len(keys))
			for i, k := range keys {
				vals[i] = str(m, k)
			}
			return a.out().Printf(resp.Body, done, vals...)
		},
	}
	addFields(c, fields)
	bf.register(c)
	return c
}
