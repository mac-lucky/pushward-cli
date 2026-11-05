//go:build !darwin && !linux && !windows

package agent

// SystemIdle is nil here: with no idle time to read, the user counts as
// away from the desk and every prompt goes to the phone.
// shortcut: the BSDs could read the terminal the way Linux does, through
// their own process table; add it when someone runs the hooks there.
func SystemIdle() Idler { return nil }
