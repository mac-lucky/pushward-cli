package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

// ErrOrgKey refuses an organization's key. Its cards and notifications reach
// everyone the organization's routing reaches, and any of them could answer
// a prompt that runs on this computer, so the hooks send nothing with it.
var ErrOrgKey = errors.New("this is an organization's key, whose cards and notifications reach other members; the agent hooks need a personal key")

// keyCheckSlug is the slug of the create that tells the kinds of key apart.
const keyCheckSlug = "agent-key-check"

// codeTargetUnavailable is the server's answer to a target from a personal
// key.
const codeTargetUnavailable = "org.target_unavailable"

// CheckKey returns ErrOrgKey for an organization's key, nil for any other.
// It asks the server once per Runner, before the first card or
// notification.
func (r *Runner) CheckKey(ctx context.Context) error {
	r.keyOnce.Do(func() { r.keyErr = r.checkKey(ctx) })
	return r.keyErr
}

// checkKey creates an activity with an empty target, which only
// organization keys may have. The server refuses it from a personal key as
// org.target_unavailable and from an organization's key as an invalid
// target, so nothing is created either way.
func (r *Runner) checkKey(ctx context.Context) error {
	_, err := r.Call(ctx, "createActivity", nil, nil, map[string]any{"slug": keyCheckSlug, "name": "PushWard agent", "target": map[string]any{}})
	var ae *api.Error
	switch {
	case errors.As(err, &ae) && ae.Code == codeTargetUnavailable:
		return nil
	case errors.As(err, &ae) && strings.HasPrefix(ae.Code, "org."):
		r.logf("an organization's key, leaving it to the terminal")
		return ErrOrgKey
	case err == nil:
		// The target was taken, so nothing tells the keys apart; take the
		// row back out.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
		defer cancel()
		_, err = r.Call(cctx, "deleteActivity", map[string]string{"slug": keyCheckSlug}, nil, nil)
		r.logf("key check took a target, removed it: %s", describe(err))
		return nil
	}
	// Refused before the target was read (no key, no plan, the key's own
	// limits, the network): what follows gets the same answer.
	r.logf("key check: %s", describe(err))
	return nil
}
