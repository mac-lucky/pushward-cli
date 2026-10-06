package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/body"
)

// A field is a convenience flag that sets one path in the request body.
// Everything a field can set is also reachable with -F, so the table only
// covers what people type often.
type field struct {
	flag  string
	path  string
	kind  kind
	usage string
}

type kind int

const (
	kString kind = iota
	kInt
	kNumber
	kSeconds     // 90, 15m, 2h, 7d -> integer seconds, at least 1
	kSecondsZero // same, 0 allowed
	kDuration    // passed through: 90 -> number, 1h30m -> string
	kUnix        // RFC 3339, unix seconds, or a duration from now -> unix seconds
	kRFC3339     // RFC 3339, unix seconds, or a duration from now -> RFC 3339 (UTC)
	kCSV         // a,b,c -> ["a","b","c"]
	kNames       // like kCSV, but an empty item is an error
	kStep        // 2 -> current_step, 2/5 -> current_step and total_steps
)

func addFields(cmd *cobra.Command, fields []field) {
	for _, f := range fields {
		cmd.Flags().String(f.flag, "", f.usage)
	}
}

// collectFields returns the body fragment for every field flag the user set.
func collectFields(cmd *cobra.Command, fields []field, now time.Time) (map[string]any, error) {
	obj := map[string]any{}
	for _, f := range fields {
		if !cmd.Flags().Changed(f.flag) {
			continue
		}
		raw, _ := cmd.Flags().GetString(f.flag)
		if err := setField(obj, f, raw, now); err != nil {
			return nil, usagef("--%s: %v", f.flag, err)
		}
	}
	return obj, nil
}

func setField(obj map[string]any, f field, raw string, now time.Time) error {
	var v any
	switch f.kind {
	case kString:
		v = raw
	case kInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("want an integer, got %q", raw)
		}
		v = n
	case kNumber:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return fmt.Errorf("want a number, got %q", raw)
		}
		v = json.Number(raw)
	case kSeconds, kSecondsZero:
		n, err := parseSeconds(raw)
		if err != nil {
			return err
		}
		if n == 0 && f.kind == kSeconds {
			return fmt.Errorf("must be at least 1s")
		}
		v = n
	case kDuration:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			v = n
		} else {
			v = raw
		}
	case kUnix:
		t, err := parseTime(raw, now)
		if err != nil {
			return err
		}
		v = t.Unix()
	case kRFC3339:
		t, err := parseTime(raw, now)
		if err != nil {
			return err
		}
		v = t.UTC().Format(time.RFC3339)
	case kCSV, kNames:
		// A blank name is an error, not skipped: dropping it could turn a
		// target or a slug restriction into "everything". A blank step label
		// is fine (that step shows N/M).
		if f.kind == kNames && strings.TrimSpace(raw) == "" {
			return fmt.Errorf("no names given; leave the flag out instead")
		}
		var list []any
		for s := range strings.SplitSeq(raw, ",") {
			s = strings.TrimSpace(s)
			if f.kind == kNames && s == "" {
				return fmt.Errorf("empty name in %q", raw)
			}
			list = append(list, s)
		}
		v = list
	case kStep:
		cur, total, hasTotal := strings.Cut(raw, "/")
		c, err := strconv.Atoi(cur)
		if err != nil || c < 0 {
			return fmt.Errorf("want N or N/TOTAL, got %q", raw)
		}
		if hasTotal {
			t, err := strconv.Atoi(total)
			if err != nil || t < 1 {
				return fmt.Errorf("want N or N/TOTAL, got %q", raw)
			}
			if err := body.Set(obj, "content.total_steps", t); err != nil {
				return err
			}
		}
		v = c
	}
	return body.Set(obj, f.path, v)
}

var dayDuration = regexp.MustCompile(`^([0-9]+)d$`)

// parseSeconds accepts plain seconds or a Go duration, plus a d suffix for days.
func parseSeconds(raw string) (int64, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("must not be negative")
		}
		return n, nil
	}
	if m := dayDuration.FindStringSubmatch(raw); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return n * 86400, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("want seconds or a duration like 90s, 15m, 2h, 7d; got %q", raw)
	}
	return int64(d.Round(time.Second) / time.Second), nil
}

// parseTime accepts RFC 3339, unix seconds, or a duration from now.
func parseTime(raw string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	if s, err := parseSeconds(raw); err == nil {
		return now.Add(time.Duration(s) * time.Second), nil
	}
	return time.Time{}, fmt.Errorf("want RFC 3339 (2026-10-01T09:00:00Z), unix seconds, or a duration from now; got %q", raw)
}

// bodyFlags are the generic body inputs every writing command takes.
type bodyFlags struct {
	data  string
	str   []string
	typed []string
}

func (b *bodyFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&b.data, "data", "d", "", "request body as JSON: a literal object, @file, or - for stdin")
	cmd.Flags().StringArrayVarP(&b.str, "field", "f", nil, "set a string field: key=value (repeatable, dotted keys)")
	cmd.Flags().StringArrayVarP(&b.typed, "raw-field", "F", nil, "set a typed field: key=value with numbers, true/false/null, JSON, @file (repeatable)")
}

// build layers the body: --data, then the convenience flags, then -f, then -F.
func (b *bodyFlags) build(a *App, conv map[string]any) (map[string]any, error) {
	obj := map[string]any{}
	if b.data != "" {
		d, err := body.ParseJSON(b.data, a.Stdin)
		if err != nil {
			return nil, usagef("%v", err)
		}
		obj = d
	}
	body.Merge(obj, conv)
	for _, f := range b.str {
		if err := body.ApplyField(obj, f, false, a.Stdin); err != nil {
			return nil, usagef("%v", err)
		}
	}
	for _, f := range b.typed {
		if err := body.ApplyField(obj, f, true, a.Stdin); err != nil {
			return nil, usagef("%v", err)
		}
	}
	return obj, nil
}

// pairs parses repeated key=value flags.
func pairs(flag string, vals []string) ([][2]string, error) {
	out := make([][2]string, 0, len(vals))
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, usagef("--%s %q: want key=value", flag, v)
		}
		out = append(out, [2]string{k, val})
	}
	return out, nil
}

// buttons turns repeated id=Title flags into the [{id, title}] list that
// notification actions and approval options share.
func buttons(flag string, vals []string) ([]any, error) {
	ps, err := pairs(flag, vals)
	if err != nil {
		return nil, err
	}
	list := make([]any, 0, len(ps))
	for _, p := range ps {
		list = append(list, map[string]any{"id": p[0], "title": p[1]})
	}
	return list, nil
}

var activityContentFields = []field{
	{"template", "content.template", kString, "template: generic, countdown, steps, alert, gauge, timeline, board, log, media, approval"},
	{"text", "content.state", kString, "status text on the card (content.state)"},
	{"subtitle", "content.subtitle", kString, "secondary line"},
	{"progress", "content.progress", kNumber, "progress from 0 to 1"},
	{"icon", "content.icon", kString, "SF Symbol name, or mdi:<name>"},
	{"color", "content.accent_color", kString, "accent color: a name (green, red, ...) or #RRGGBB"},
	{"url", "content.url", kString, "URL the card's button opens"},
	{"image", "content.image_url", kString, "https image shown beside the icon (generic, steps, media)"},
	{"compact-label", "content.compact_label", kString, "up to 4 characters for the Dynamic Island"},
	{"duration", "content.duration", kDuration, "expected length (90, 5m, 1h30m); sets start and end dates"},
	{"eta", "content.end_date", kUnix, "expected finish: RFC 3339, unix seconds, or a duration from now"},
	{"remaining", "content.remaining_time", kSecondsZero, "remaining time in seconds or as a duration"},
	{"step", "content.current_step", kStep, "steps template: current step, or N/TOTAL"},
	{"step-labels", "content.step_labels", kCSV, "steps template: comma-separated labels"},
	{"value", "content.value", kNumber, "gauge template: current value"},
	{"min", "content.min_value", kNumber, "gauge template: range minimum"},
	{"max", "content.max_value", kNumber, "gauge template: range maximum"},
	{"unit", "content.unit", kString, "unit label (gauge, timeline)"},
	{"severity", "content.severity", kString, "alert template: critical, warning, or info"},
}

var activityLifecycleFields = []field{
	{"priority", "priority", kInt, "eviction priority, 0 to 10"},
	{"ended-ttl", "ended_ttl", kSeconds, "delete this long after it ends (e.g. 15m, 1d)"},
	{"stale-ttl", "stale_ttl", kSeconds, "end it after this long without an update (e.g. 6h)"},
	{"dismissal-ttl", "dismissal_ttl", kSecondsZero, "keep it on the Lock Screen this long after it ends (max 4h)"},
}

// targetFields narrow who an organization key's activity or notification
// reaches. A personal account's key gets a 422 for them.
var targetFields = []field{
	{"target-groups", "target.groups", kNames, "organization keys: comma-separated names of the groups that get it"},
	{"target-tags", "target.tags", kNames, "organization keys: comma-separated device tag names; devices with any of them get it"},
	{"target-members", "target.members", kNames, "organization keys: comma-separated user ids of the members that get it"},
}

var notificationFields = slices.Concat(notificationBaseFields, ackFields, targetFields)

// ackFields tune a notification that repeats until acknowledged. Setting any
// acknowledge.* path asks for the acknowledgement, the same as --ack.
var ackFields = []field{
	{"ack-repeat", "acknowledge.repeat_seconds", kSeconds, "repeat until acknowledged, this often: 30s to 1h (default 1m); implies --ack"},
	{"ack-expire", "acknowledge.expire_seconds", kSeconds, "stop repeating after this long: 1m to 3h (default 1h); implies --ack"},
	{"ack-title", "acknowledge.action_title", kString, "title of the acknowledge button (default Acknowledge); implies --ack"},
	{"callback-url", "callback_url", kString, "with --ack: https URL that gets a signed POST when it is acknowledged or expires"},
}

var notificationBaseFields = []field{
	{"title", "title", kString, "title (required)"},
	{"subtitle", "subtitle", kString, "subtitle"},
	{"level", "level", kString, "interruption level: passive, active, time-sensitive, critical"},
	{"url", "url", kString, "URL opened when the notification is tapped"},
	{"icon-url", "icon_url", kString, "avatar image URL"},
	{"thread", "thread_id", kString, "thread id for grouping"},
	{"collapse-id", "collapse_id", kString, "collapse id: a newer notification with the same id replaces this one"},
	{"source", "source", kString, "source identifier"},
	{"source-name", "source_display_name", kString, "source name shown to the user"},
	{"activity", "activity_slug", kString, "link to an activity by slug"},
	{"volume", "volume", kNumber, "critical alert volume from 0 to 1"},
}

var widgetContentFields = []field{
	{"template", "content.template", kString, "template: value, progress, status, gauge, stat_list, trend, countdown, battery, schedule, flow"},
	{"value", "content.value", kNumber, "numeric value"},
	{"label", "content.label", kString, "label"},
	{"unit", "content.unit", kString, "unit label"},
	{"subtitle", "content.subtitle", kString, "secondary line"},
	{"icon", "content.icon", kString, "SF Symbol name, or mdi:<name>"},
	{"color", "content.accent_color", kString, "accent color: a name or #RRGGBB"},
	{"min", "content.min_value", kNumber, "range minimum"},
	{"max", "content.max_value", kNumber, "range maximum"},
	{"severity", "content.severity", kString, "status severity"},
	{"trend", "content.trend", kString, "trend arrow: up, down, flat"},
}

var widgetMetaFields = []field{
	{"name", "name", kString, "name shown in the widget picker"},
	{"push-throttle", "push_throttle", kSeconds, "minimum time between pushes for this widget"},
	{"stale-after", "stale_after", kSeconds, "render as stale this long after the last update"},
}
