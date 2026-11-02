package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

// ErrOrgKey refuses an organization's key. Its cards and notifications reach
// everyone the organization's routing reaches, and any of them could answer
// a prompt that runs on this computer, so the hooks send nothing with it.
var ErrOrgKey = errors.New("this is an organization's key, whose cards and notifications reach other members; the agent hooks need a personal key")

// ErrKeyUnchecked is a key the check could not tell apart: both creates were
// refused before the server read their target, so it may be an
// organization's, and the hooks send nothing with it.
var ErrKeyUnchecked = errors.New("could not tell whether this is a personal key; the agent hooks send nothing with it")

// keyCheckSlug is the slug of the create that tells the kinds of key apart,
// and the collapse id of the notification that does when the create cannot.
const keyCheckSlug = "agent-key-check"

// codeTargetUnavailable is the server's answer to a target from a personal
// key.
const codeTargetUnavailable = "org.target_unavailable"

// codeTargetDenied refuses a target outside a key's groups and tags, which
// only an organization's key can have.
const codeTargetDenied = "integration_key.target_denied"

// CheckKey returns nil for a personal key, ErrOrgKey for an organization's
// and ErrKeyUnchecked when the server's answers do not say which. It asks
// the server once per Runner, before the first card or notification.
func (r *Runner) CheckKey(ctx context.Context) error {
	r.keyOnce.Do(func() { r.keyErr = r.checkKey(ctx) })
	return r.keyErr
}

// checkKey creates an activity with an empty target, which only
// organization keys may have. The server refuses it from a personal key as
// org.target_unavailable and from an organization's key as an invalid
// target, so nothing is created either way.
//
// A key may be refused the activity before the server reads the target
// (it may not manage activities, the plan, its own limits) and still send
// notifications, which the hooks fall back to. Such a refusal says nothing
// about the key, so a notification with an empty target asks the same
// question; when that is refused before its target too, nothing is sent.
func (r *Runner) checkKey(ctx context.Context) error {
	_, err := r.Call(ctx, "createActivity", nil, nil, map[string]any{"slug": keyCheckSlug, "name": "PushWard agent", "target": map[string]any{}})
	if err == nil {
		// The target was taken, so nothing tells the keys apart; take the
		// row back out.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
		defer cancel()
		_, err = r.Call(cctx, "deleteActivity", map[string]string{"slug": keyCheckSlug}, nil, nil)
		r.logf("key check took a target, removed it: %s", describe(err))
		return nil
	}
	if known, kind := r.keyKind(err); known {
		return kind
	}
	r.logf("key check: %s", describe(err))
	var ae *api.Error
	if !errors.As(err, &ae) {
		// No answer from the server (no key, the network): the next
		// request would get none either.
		return fmt.Errorf("%w (%s)", ErrKeyUnchecked, describe(err))
	}

	_, err = r.Call(ctx, "createNotification", nil, nil, map[string]any{
		"title": "PushWard agent", "body": "Key check", "level": "passive",
		"collapse_id": keyCheckSlug, "target": map[string]any{},
	})
	if err == nil {
		// Sent with the target ignored, so nothing tells the keys apart;
		// take it back off the devices.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
		defer cancel()
		_, err = r.Call(cctx, "retractNotifications", nil, nil, map[string]any{"collapse_id": keyCheckSlug})
		r.logf("key check sent a notification, retracted it: %s", describe(err))
		return nil
	}
	if known, kind := r.keyKind(err); known {
		return kind
	}
	r.logf("key check: %s, sending nothing", describe(err))
	return fmt.Errorf("%w (%s)", ErrKeyUnchecked, describe(err))
}

// keyKind reads the refusal of a create with an empty target. known reports
// whether it tells the kinds of key apart; kind is then nil for a personal
// key and ErrOrgKey for an organization's.
func (r *Runner) keyKind(err error) (known bool, kind error) {
	var ae *api.Error
	switch {
	case !errors.As(err, &ae):
		return false, nil
	case ae.Code == codeTargetUnavailable:
		return true, nil
	case strings.HasPrefix(ae.Code, "org."), ae.Code == codeTargetDenied:
		r.logf("an organization's key, leaving it to the terminal")
		return true, ErrOrgKey
	}
	return false, nil
}
