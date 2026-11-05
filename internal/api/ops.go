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
	{"listActivityPermissions", "GET", "/activities/{slug}/permissions", false},
	{"grantActivityPermission", "POST", "/activities/{slug}/permissions", false},
	{"getOnboarding", "GET", "/app/onboarding", false},
	{"updateOnboarding", "PATCH", "/app/onboarding", false},
	{"getMe", "GET", "/auth/me", false},
	{"updateMe", "PATCH", "/auth/me", false},
	{"listDevices", "GET", "/devices", false},
	{"getDevice", "GET", "/devices/{device_id}", false},
	{"updateDevice", "PATCH", "/devices/{device_id}", false},
	{"deleteDevice", "DELETE", "/devices/{device_id}", false},
	{"listSubscriptions", "GET", "/devices/{device_id}/subscriptions", false},
	{"getDeviceSubscription", "GET", "/devices/{device_id}/subscriptions/{slug}", false},
	{"subscribeDevice", "POST", "/devices/{device_id}/subscriptions/{slug}", false},
	{"unsubscribeDevice", "DELETE", "/devices/{device_id}/subscriptions/{slug}", false},
	{"sendEmail", "POST", "/emails", false},
	{"listEmailRecipients", "GET", "/emails/recipients", false},
	{"addEmailRecipient", "POST", "/emails/recipients", false},
	{"deleteEmailRecipient", "DELETE", "/emails/recipients/{id}", false},
	{"resendEmailVerification", "POST", "/emails/recipients/{id}/resend", false},
	{"getHealth", "GET", "/health", true},
	{"listIntegrationKeys", "GET", "/integrations/keys", false},
	{"createIntegrationKey", "POST", "/integrations/keys", false},
	{"updateIntegrationKey", "PATCH", "/integrations/keys/{key_id}", false},
	{"revokeIntegrationKey", "DELETE", "/integrations/keys/{key_id}", false},
	{"rollIntegrationKey", "POST", "/integrations/keys/{key_id}/roll", false},
	{"createNotification", "POST", "/notifications", false},
	{"listNotificationAnswers", "GET", "/notifications/answers", false},
	{"getNotificationAnswer", "GET", "/notifications/answers/{id}", false},
	{"cancelNotificationReceiptsByTag", "POST", "/notifications/receipts/cancel", false},
	{"getNotificationReceipt", "GET", "/notifications/receipts/{id}", false},
	{"cancelNotificationReceipt", "POST", "/notifications/receipts/{id}/cancel", false},
	{"retractNotifications", "POST", "/notifications/retract", false},
	{"listScheduledNotifications", "GET", "/notifications/scheduled", false},
	{"createScheduledNotification", "POST", "/notifications/scheduled", false},
	{"getScheduledNotification", "GET", "/notifications/scheduled/{id}", false},
	{"cancelScheduledNotification", "DELETE", "/notifications/scheduled/{id}", false},
	{"listOrgs", "GET", "/orgs", false},
	{"getOrg", "GET", "/orgs/{org_id}", false},
	{"updateOrg", "PATCH", "/orgs/{org_id}", false},
	{"listOrgAuditEvents", "GET", "/orgs/{org_id}/audit-events", false},
	{"getOrgBilling", "GET", "/orgs/{org_id}/billing", false},
	{"listOrgInvoices", "GET", "/orgs/{org_id}/billing/invoices", false},
	{"getOrgInvoicePdf", "GET", "/orgs/{org_id}/billing/invoices/{transaction_id}/pdf", false},
	{"listOrgDevices", "GET", "/orgs/{org_id}/devices", false},
	{"listOrgGroups", "GET", "/orgs/{org_id}/groups", false},
	{"createOrgGroup", "POST", "/orgs/{org_id}/groups", false},
	{"getOrgGroup", "GET", "/orgs/{org_id}/groups/{group_id}", false},
	{"updateOrgGroup", "PATCH", "/orgs/{org_id}/groups/{group_id}", false},
	{"deleteOrgGroup", "DELETE", "/orgs/{org_id}/groups/{group_id}", false},
	{"setOrgGroupMembers", "PUT", "/orgs/{org_id}/groups/{group_id}/members", false},
	{"listOrgInvites", "GET", "/orgs/{org_id}/invites", false},
	{"createOrgInvite", "POST", "/orgs/{org_id}/invites", false},
	{"getOrgInvite", "GET", "/orgs/{org_id}/invites/{invite_id}", false},
	{"revokeOrgInvite", "DELETE", "/orgs/{org_id}/invites/{invite_id}", false},
	{"listOrgKeys", "GET", "/orgs/{org_id}/keys", false},
	{"createOrgKey", "POST", "/orgs/{org_id}/keys", false},
	{"getOrgKey", "GET", "/orgs/{org_id}/keys/{key_id}", false},
	{"updateOrgKey", "PATCH", "/orgs/{org_id}/keys/{key_id}", false},
	{"revokeOrgKey", "DELETE", "/orgs/{org_id}/keys/{key_id}", false},
	{"rollOrgKey", "POST", "/orgs/{org_id}/keys/{key_id}/roll", false},
	{"listOrgMembers", "GET", "/orgs/{org_id}/members", false},
	{"getOrgMember", "GET", "/orgs/{org_id}/members/{user_id}", false},
	{"updateOrgMember", "PATCH", "/orgs/{org_id}/members/{user_id}", false},
	{"deleteOrgMember", "DELETE", "/orgs/{org_id}/members/{user_id}", false},
	{"setOrgDeviceTags", "PUT", "/orgs/{org_id}/members/{user_id}/devices/{device_id}/tags", false},
	{"listOrgRoutes", "GET", "/orgs/{org_id}/routes", false},
	{"createOrgRoute", "POST", "/orgs/{org_id}/routes", false},
	{"applyOrgRoutes", "POST", "/orgs/{org_id}/routes/apply", false},
	{"reorderOrgRoutes", "PUT", "/orgs/{org_id}/routes/order", false},
	{"getOrgRoute", "GET", "/orgs/{org_id}/routes/{route_id}", false},
	{"updateOrgRoute", "PATCH", "/orgs/{org_id}/routes/{route_id}", false},
	{"deleteOrgRoute", "DELETE", "/orgs/{org_id}/routes/{route_id}", false},
	{"listOrgTags", "GET", "/orgs/{org_id}/tags", false},
	{"createOrgTag", "POST", "/orgs/{org_id}/tags", false},
	{"getOrgTag", "GET", "/orgs/{org_id}/tags/{tag_id}", false},
	{"updateOrgTag", "PATCH", "/orgs/{org_id}/tags/{tag_id}", false},
	{"deleteOrgTag", "DELETE", "/orgs/{org_id}/tags/{tag_id}", false},
	{"updateOrgTagDevices", "POST", "/orgs/{org_id}/tags/{tag_id}/devices", false},
	{"listPermissions", "GET", "/permissions", false},
	{"grantPatternPermission", "POST", "/permissions/patterns", false},
	{"updatePermission", "PATCH", "/permissions/{id}", false},
	{"deletePermission", "DELETE", "/permissions/{id}", false},
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
