//go:build !darwin

package agent

// SystemIdle is nil off macOS: with no idle time to read, the user counts as
// away from the desk and every prompt goes to the phone.
func SystemIdle() Idler { return nil }
