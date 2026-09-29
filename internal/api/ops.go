package api

import (
	"fmt"
	"net/url"
	"strings"
)

// Operation is one endpoint of the public API, keyed by its OpenAPI
// operationId. Commands address endpoints only through this table, and the
// spec parity test holds the table equal to openapi.yaml, so a route the
// server adds or renames fails the build instead of going unnoticed.
type Operation struct {
	ID     string
	Method string
	Path   string
	// Public operations need no key; the spec marks them by leaving out
	// security.
	Public bool
}

var Operations = []Operation{
	{"listActivities", "GET", "/activities", false},
	{"createActivity", "POST", "/activities", false},
	{"getActivity", "GET", "/activities/{slug}", false},
	{"updateActivity", "PATCH", "/activities/{slug}", false},
	{"deleteActivity", "DELETE", "/activities/{slug}", false},
	{"getMe", "GET", "/auth/me", false},
	{"sendEmail", "POST", "/emails", false},
	{"getHealth", "GET", "/health", true},
	{"createNotification", "POST", "/notifications", false},
	{"getNotificationAnswer", "GET", "/notifications/answers/{id}", false},
	{"listScheduledNotifications", "GET", "/notifications/scheduled", false},
	{"createScheduledNotification", "POST", "/notifications/scheduled", false},
	{"getScheduledNotification", "GET", "/notifications/scheduled/{id}", false},
	{"cancelScheduledNotification", "DELETE", "/notifications/scheduled/{id}", false},
	{"listWidgets", "GET", "/widgets", false},
	{"createWidget", "POST", "/widgets", false},
	{"getWidget", "GET", "/widgets/{slug}", false},
	{"updateWidget", "PATCH", "/widgets/{slug}", false},
	{"deleteWidget", "DELETE", "/widgets/{slug}", false},
}

// Op returns the operation with the given id. An unknown id is a programming
// error, so it panics rather than returning something a caller could ignore.
func Op(id string) Operation {
	for _, op := range Operations {
		if op.ID == id {
			return op
		}
	}
	panic("api: unknown operation " + id)
}

// Expand fills the {name} placeholders of the operation's path. Values are
// path-escaped, so a slug can never reach a different route.
func (o Operation) Expand(params map[string]string) (string, error) {
	path := o.Path
	for {
		start := strings.IndexByte(path, '{')
		if start < 0 {
			return path, nil
		}
		end := strings.IndexByte(path[start:], '}')
		if end < 0 {
			return "", fmt.Errorf("%s: unterminated placeholder in %s", o.ID, o.Path)
		}
		name := path[start+1 : start+end]
		value, ok := params[name]
		if !ok || value == "" {
			return "", fmt.Errorf("%s: missing path parameter %q", o.ID, name)
		}
		path = path[:start] + url.PathEscape(value) + path[start+end+1:]
	}
}
