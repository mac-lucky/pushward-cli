package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// waitingText is the notice for a prompt left in the terminal.
const waitingText = "A prompt is waiting in the terminal"

// needsInput sends a notice when a background session starts waiting on the
// user, or a prompt is left in the terminal while the user is away. Claude
// Code sends permission_prompt once per dialog, 6 seconds after it asks,
// while the PermissionRequest hook may still be waiting. A dialog whose hook
// counted the user as away, or that already got its notice, left a dialog
// file, and permission_prompt takes it instead of pushing again: that hook
// tells the user itself if the prompt ends up in the terminal. A dialog that
// showed while the user was at the desk left none and stays silent after
// they leave, unless they left within those 6 seconds.
// idle_prompt, which an older install still asks for, is not passed on: it
// comes about a minute after Stop, and with the same collapse id its generic
// text would replace the finished notice and push a second time.
func (r *Runner) needsInput(ctx context.Context, in Input) {
	switch in.NotificationType {
	case "agent_needs_input":
		r.notice(ctx, in, Claude.Name()+" needs you", in.Message, "Waiting for your input")
	case "permission_prompt":
		if r.dialogs(in, true) {
			r.logf("%s: notice: left to the dialog's own hook", in.noticeID())
			return
		}
		// At the desk the user sees the dialog, even with --notices always.
		if r.Opts.Notices == "always" && r.atDesk(ctx) {
			return
		}
		r.notice(ctx, in, Claude.Name()+" needs you", in.Message, waitingText)
	}
}

// noticeWithin bounds a notice sent from a hook the agent waits on before it
// shows its own dialog, so a slow API does not hold the dialog up.
const noticeWithin = 5 * time.Second

// heldNotice is a "needs you" notice sent from such a hook.
func (r *Runner) heldNotice(ctx context.Context, agent Agent, in Input, text, fallback string) bool {
	ctx, cancel := context.WithTimeout(ctx, noticeWithin)
	defer cancel()
	return r.notice(ctx, in, agent.Name()+" needs you", text, fallback)
}

// dialogNotice sends the notice for a dialog the hook leaves to the terminal
// (a plan review, a question it cannot ask) and leaves a dialog file for it:
// Claude Code sends permission_prompt for that dialog 6 seconds after showing
// it, which would push a second time under the same collapse id.
func (r *Runner) dialogNotice(ctx context.Context, in Input, text, fallback string) {
	// The file goes first, so a slow send still finds it in place; a notice
	// that did not go out leaves the permission_prompt to try.
	if path := r.addDialog(in); !r.heldNotice(ctx, Claude, in, text, fallback) && path != "" {
		_ = os.Remove(path)
	}
}

// dialogFor is how long a dialog file stands for the permission_prompt that
// follows its dialog.
const dialogFor = time.Minute

// answeredFor is how long it stands once its hook has the answer, for a
// permission_prompt Claude Code sent just before it, whose hook may be slow
// to start. Under 6 seconds: the session's next dialog starts after the
// answer, so its permission_prompt comes 6 seconds later at the earliest.
const answeredFor = 5 * time.Second

// answerDialog writes the time of the answer into a dialog file the
// permission_prompt has not taken yet.
func (r *Runner) answerDialog(path string) {
	if path == "" {
		return
	}
	// No O_CREATE: a file already taken stays gone.
	f, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- the file addDialog made in the Dialogs directory
	if err != nil {
		return
	}
	_, err = f.WriteString(strconv.FormatInt(time.Now().UnixNano(), 10))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		r.logf("dialog: %s", describe(err))
	}
}

// standing reports whether a dialog file made at made still stands at now:
// empty while its hook waits, it holds the time of the answer once there is
// one. Content that is no time after made is that time still being written.
func standing(path string, made, now time.Time) bool {
	b, err := os.ReadFile(path) // #nosec G304 -- a file listed in the Dialogs directory
	if err != nil {
		return false
	}
	if len(b) == 0 {
		return true
	}
	ns, err := strconv.ParseInt(string(b), 10, 64)
	at := time.Unix(0, ns)
	return err != nil || at.Before(made) || now.Sub(at) < answeredFor
}

// addDialog leaves a file for one dialog of this session in the Dialogs
// directory, named <notice id>-<unix nanos>-<random>, and returns its path.
// One file per dialog, so hooks running at the same time never rewrite each
// other's.
func (r *Runner) addDialog(in Input) (path string) {
	if r.Dialogs == "" {
		return ""
	}
	r.dialogs(in, false)
	err := os.MkdirAll(r.Dialogs, 0o700)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(r.Dialogs, in.noticeID()+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-*"); err == nil {
			path = f.Name()
			err = f.Close()
		}
	}
	if err != nil {
		r.logf("%s: dialog: %s", in.noticeID(), describe(err))
	}
	return path
}

// dialogs removes the files older than dialogFor, of any session, and with
// take one fresh file of this session's, reporting whether it took one.
// Taking is the remove that succeeds, so two hooks never take the same file.
func (r *Runner) dialogs(in Input, take bool) (took bool) {
	if r.Dialogs == "" {
		return false
	}
	entries, err := os.ReadDir(r.Dialogs)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.logf("%s: dialog: %s", in.noticeID(), describe(err))
		}
		return false
	}
	// Read after the listing: no file in it is younger than now.
	now := time.Now()
	mine := in.noticeID() + "-"
	for _, e := range entries {
		path := filepath.Join(r.Dialogs, e.Name())
		made := dialogTime(e.Name())
		switch age := now.Sub(made); {
		case age < 0 || age >= dialogFor:
			_ = os.Remove(path)
		case take && !took && strings.HasPrefix(e.Name(), mine) && standing(path, made, now):
			took = os.Remove(path) == nil
		}
	}
	return took
}

// dialogTime reads the time out of a dialog file's name; the zero time, which
// counts as stale, for a name that has none.
func dialogTime(name string) time.Time {
	f := strings.Split(name, "-")
	if len(f) != 4 {
		return time.Time{}
	}
	ns, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, ns)
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
// desk unless --notices always, and reports whether it went out. A session's
// notices share a collapse id, so the newest replaces the one before. A turn
// that ends with the user at the desk takes the last one back instead.
func (r *Runner) notice(ctx context.Context, in Input, title, text, fallback string) bool {
	if r.Opts.Notices == "off" {
		return false
	}
	if r.Opts.Notices != "always" && r.atDesk(ctx) {
		if in.Event == "Stop" {
			r.takeBackNotice(ctx, in)
		}
		return false
	}
	if r.CheckKey(ctx) != nil {
		return false
	}
	body := cut(oneLine(maskSecrets(text)), 200)
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
	if err != nil {
		return false
	}
	r.markNotice(in)
	return true
}

// noticeFor is how long a notice marker stands: a session that ends no turn
// at the desk for that long keeps its last notice on the devices.
const noticeFor = 24 * time.Hour

// noticeMarker is the file that says a notice of this session may still be
// up, named by its collapse id in a directory beside Dialogs; "" when there
// is no Dialogs directory.
func (r *Runner) noticeMarker(in Input) string {
	if r.Dialogs == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(r.Dialogs), "agent-notices", in.noticeID())
}

// markNotice leaves this session's marker, rewritten so its time is the
// last notice's, and removes the markers older than noticeFor, of any
// session.
func (r *Runner) markNotice(in Input) {
	path := r.noticeMarker(in)
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) >= noticeFor {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}
	if err != nil {
		r.logf("%s: notice marker: %s", in.noticeID(), describe(err))
	}
}

// takeBackNotice retracts this session's notice when its marker says one
// went out. Taking the marker is the remove that succeeds, so two hooks
// never both retract. The retract has the cleanup budget, like the prompt's.
func (r *Runner) takeBackNotice(ctx context.Context, in Input) {
	path := r.noticeMarker(in)
	if path == "" || os.Remove(path) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
	defer cancel()
	_, err := r.Call(ctx, "retractNotifications", nil, nil, map[string]any{"collapse_id": in.noticeID()})
	r.logf("%s: retracted the notice: %s", in.noticeID(), describe(err))
}
