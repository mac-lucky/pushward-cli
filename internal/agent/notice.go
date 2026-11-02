package agent

import "context"

// needsInput sends a notice when a background session starts waiting on the
// user. A permission prompt is not one: the prompt itself goes to the phone.
// Neither is idle_prompt, which an older install still sends: it comes about
// a minute after Stop, and with the same collapse id its generic text would
// replace the finished notice and push a second time.
func (r *Runner) needsInput(ctx context.Context, in Input) {
	if in.NotificationType == "agent_needs_input" {
		r.notice(ctx, in, Claude.Name()+" needs you", in.Message, "Waiting for your input")
	}
}

// finished sends a notice when a turn ends, with the agent's last message.
func (r *Runner) finished(ctx context.Context, agent Agent, in Input) {
	msg := ""
	if in.LastAssistantMessage != nil {
		msg = *in.LastAssistantMessage
	}
	r.notice(ctx, in, agent.Name()+" finished", msg, "Done")
}

// notice sends a plain notification, only while the user is away from the
// desk unless --notices always. A session's notices share a collapse id, so
// the newest replaces the one before.
func (r *Runner) notice(ctx context.Context, in Input, title, text, fallback string) {
	if r.Opts.Notices == "off" || (r.Opts.Notices != "always" && r.atDesk(ctx)) || r.CheckKey(ctx) != nil {
		return
	}
	body := cut(oneLine(text), 200)
	if body == "" {
		body = fallback
	}
	n := map[string]any{
		"title":       title,
		"body":        body,
		"level":       "active",
		"collapse_id": in.noticeID(),
		"thread_id":   threadID,
	}
	if repo := in.repo(); repo != "" {
		n["subtitle"] = repo
	}
	_, err := r.Call(ctx, "createNotification", nil, nil, n)
	r.logf("%s: notice: %s", in.noticeID(), describe(err))
}
