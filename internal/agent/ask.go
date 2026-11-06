package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

const (
	// startWindow is how long a new card has for a device to report that it
	// started it. After that, with none reporting, the question also goes
	// out as a notification: iOS drops a push-to-start silently when a phone
	// is over its budget. An account with no Apple device to start it on
	// gets the notification at once.
	startWindow = 15 * time.Second
	// startReads is how many times a start check reads the card, a few
	// seconds apart, before a transient failure makes it give up.
	startReads = 3
	// startReadWithin bounds each of those reads.
	startReadWithin = 10 * time.Second
	// longPollSeconds is the ?wait= of every read, the most the API holds.
	longPollSeconds = "25"
	// pollEvery paces plain reads once the account's long-poll slots are
	// taken (it has about four, shared by every waiting reader).
	pollEvery = 3 * time.Second
	// A long-poll that answers sooner than quickReturn with nothing new
	// waits quickPause before the next, so it cannot spin.
	quickReturn = time.Second
	quickPause  = 2 * time.Second
	// cleanupWithin bounds ending the card and retracting the notification,
	// so a cancelled hook still exits promptly.
	cleanupWithin = 2 * time.Second
	// threadID groups the agent's notifications on the devices.
	threadID = "pushward-agent"
	// endedTTL is how long, in seconds, an ended card stays on the account.
	endedTTL = 60
)

type outcome int

const (
	answered outcome = iota
	// handedBack: input at the Mac, or nothing left to ask on.
	handedBack
	timedOut
	cancelled
)

// option is one answer the user can pick.
type option struct {
	id string
	// title fits the card's button (24 runes, masked); label is the full
	// text, the answer given to the agent and, masked, the notification's
	// button.
	title, label string
	style, icon  string
	destructive  bool
	// unlock asks for the device to be unlocked before the answer counts.
	unlock bool
}

// round is one question put to the user, on the card and, when that is not
// enough, in a notification.
type round struct {
	question string // the card's text
	title    string // the notification's title
	body     string // the notification's body
	options  []option
	rows     []detail
	// other is the id of the notification's typed-answer action, "" for
	// none.
	other string
	// step and steps place the round in a multi-question card; steps is 0
	// for a card with one question.
	step, steps int
}

// fitsCard reports whether an approval card can carry the round's options:
// two to four, which its buttons tell apart. Two that cut to the same title
// go out as a notification, whose buttons have room for more of the label.
func (rd round) fitsCard() bool {
	if len(rd.options) < 2 || len(rd.options) > 4 {
		return false
	}
	for i, o := range rd.options {
		for _, p := range rd.options[:i] {
			if strings.EqualFold(o.title, p.title) {
				return false
			}
		}
	}
	return true
}

// final reports whether the server ends the card once this round is
// answered on it.
func (rd round) final() bool {
	return rd.steps < 2 || rd.step == rd.steps
}

func (rd round) ids() map[string]bool {
	ids := make(map[string]bool, len(rd.options)+1)
	for _, o := range rd.options {
		ids[o.id] = true
	}
	if rd.other != "" {
		ids[rd.other] = true
	}
	return ids
}

// reply is the user's answer to a round: an option, or text they typed.
type reply struct {
	option string
	text   string
}

type surface int

const (
	fromCard surface = iota
	fromNotification
)

// event is what a watcher reports: an answer, or that its surface will not
// answer this round.
type event struct {
	from  surface
	ok    bool
	reply reply
	// ended: the card has ended on the server, or is gone, so there is
	// nothing to end.
	ended bool
}

// ask is one prompt, with its card and notifications, from the first round
// to the last.
type ask struct {
	r        *Runner
	agent    Agent
	in       Input
	slug     string
	deadline time.Time

	// cardUp: the card is on the server and has not ended.
	cardUp bool
	// noCard: rounds no longer go to the card (refused, gone, or the user
	// chose notifications).
	noCard    bool
	cardSince time.Time
	// confirmed: a device reported starting the card, or it was answered.
	confirmed bool
	// targetsRead: the card was read once right after its start, for how
	// many devices it reaches.
	targetsRead bool
	// alsoNotify: no device started the card, so every round goes out as a
	// notification as well.
	alsoNotify bool
	// notifUp: a notification of this ask may still be on the devices.
	notifUp bool
	// returned: handed back because the user came back to the desk.
	returned bool
}

func (r *Runner) newAsk(agent Agent, in Input) *ask {
	return &ask{
		r:        r,
		agent:    agent,
		in:       in,
		slug:     in.slug(),
		deadline: time.Now().Add(r.Opts.MaxWait),
		noCard:   r.Opts.Via == "notification",
	}
}

// run asks the rounds in order. It returns a reply per round, or why it
// stopped, and leaves no card or notification behind that still asks.
func (a *ask) run(ctx context.Context, rounds []round) ([]reply, outcome) {
	if a.r.CheckKey(ctx) != nil {
		return nil, handedBack
	}
	defer a.withdraw(ctx)
	replies := make([]reply, 0, len(rounds))
	for _, rd := range rounds {
		rep, out := a.round(ctx, rd)
		if out != answered {
			return nil, out
		}
		replies = append(replies, rep)
	}
	return replies, answered
}

// round asks one question and waits for the first answer from the card or
// the notification, input at the desk, the deadline or cancellation.
func (a *ask) round(ctx context.Context, rd round) (reply, outcome) {
	onCard := !a.noCard && rd.fitsCard() && a.show(ctx, rd)
	if !onCard && a.r.Opts.Via == "card" {
		a.r.logf("%s: nothing to show on the card, leaving it to the terminal", a.slug)
		return reply{}, handedBack
	}
	notification := ""
	if !onCard || a.alsoNotify {
		var ok bool
		if notification, ok = a.notify(ctx, rd); !ok && !onCard {
			return reply{}, handedBack
		}
	} else if a.notifUp {
		// Nothing replaces the last round's notification this time.
		a.retract(ctx)
	}

	wctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
	}()
	// At most a card and a notification report per round.
	events := make(chan event, 2)
	open := 0
	ids := rd.ids()
	watch := func(from surface, w func(context.Context) event) {
		open++
		wg.Go(func() {
			// A panic here would end the process with exit 2, which a
			// PreToolUse hook reports as "block the tool".
			ev := event{from: from}
			defer func() {
				if recover() != nil {
					a.r.logf("%s: internal error while waiting", a.slug)
					ev = event{from: from}
				}
				if wctx.Err() == nil {
					events <- ev
				}
			}()
			ev = w(wctx)
		})
	}
	watchNotification := func() {
		id := notification
		watch(fromNotification, func(ctx context.Context) event { return a.watchNotification(ctx, id, ids) })
	}
	if onCard {
		watch(fromCard, func(ctx context.Context) event { return a.watchCard(ctx, ids) })
	}
	if notification != "" {
		watchNotification()
	}

	deadline := time.NewTimer(time.Until(a.deadline))
	defer deadline.Stop()
	// The start check reads the card right after it starts, then once the
	// start window has passed, in the background so an answer is not held
	// up behind a slow read.
	var check <-chan time.Time
	var checkTimer *time.Timer
	arm := func(at time.Time) {
		checkTimer = time.NewTimer(time.Until(at))
		check = checkTimer.C
	}
	defer func() {
		if checkTimer != nil {
			checkTimer.Stop()
		}
	}()
	starts := make(chan startCount, 1)
	if onCard && !a.confirmed && notification == "" && a.r.Opts.Via == "auto" {
		if a.targetsRead {
			arm(a.cardSince.Add(startWindow))
		} else {
			arm(time.Now())
		}
	}
	var desk <-chan time.Time
	if a.r.watchesDesk() {
		t := time.NewTicker(deskEvery)
		defer t.Stop()
		desk = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return reply{}, cancelled
		case <-deadline.C:
			a.r.logf("%s: no answer in %s", a.slug, a.r.Opts.MaxWait)
			return reply{}, timedOut
		case <-desk:
			if a.r.backAtDesk(ctx) {
				a.r.logf("%s: input at the desk, handing the prompt back to the terminal", a.slug)
				a.returned = true
				return reply{}, handedBack
			}
		case <-check:
			check = nil
			if notification == "" {
				wg.Go(func() { starts <- a.startCounts(wctx) })
			}
		case sc := <-starts:
			first := !a.targetsRead
			a.targetsRead = true
			switch {
			case notification != "":
				// The card ended meanwhile and the notification is out.
				continue
			case !sc.ok:
				// Read again once the window has passed, or give up: a card
				// whose counts the server leaves out (an organization's)
				// counts as started.
				if first {
					arm(a.cardSince.Add(startWindow))
				}
				continue
			case sc.started > 0:
				a.confirmed = true
				continue
			case sc.targeted == 0:
				a.r.logf("%s: no device to start the card on, sending a notification too", a.slug)
			case first:
				arm(a.cardSince.Add(startWindow))
				continue
			default:
				a.r.logf("%s: no device started the card (%d targeted), sending a notification too", a.slug, sc.targeted)
			}
			a.alsoNotify = true
			var ok bool
			if notification, ok = a.notify(ctx, rd); ok {
				watchNotification()
			}
		case ev := <-events:
			if ev.ok {
				a.won(ev, rd)
				return ev.reply, answered
			}
			open--
			if ev.from == fromCard {
				a.noCard = true
				a.cardUp = a.cardUp && !ev.ended
				if notification == "" && a.r.Opts.Via == "auto" {
					var ok bool
					if notification, ok = a.notify(ctx, rd); ok {
						watchNotification()
					}
				}
			}
			if open == 0 {
				a.r.logf("%s: no surface left to answer on, leaving it to the terminal", a.slug)
				return reply{}, handedBack
			}
		}
	}
}

// won records which surface answered. The other one stays up until the
// next round replaces it or withdraw takes it down.
func (a *ask) won(ev event, rd round) {
	switch ev.from {
	case fromCard:
		a.r.logf("%s: answered on the card", a.slug)
		// The card reaches a device, so later rounds need no notification.
		a.confirmed, a.alsoNotify = true, false
		switch {
		case rd.final():
			// The server ends the card after its last answer.
			a.cardUp = false
		case ev.ended:
			// A server without steps ends the card at the first answer:
			// the questions left go out as notifications.
			a.r.logf("%s: the card ended after question %d of %d", a.slug, rd.step, rd.steps)
			a.cardUp, a.noCard = false, true
		}
	case fromNotification:
		a.r.logf("%s: answered in the notification", a.slug)
		// Not retracted: the same call asked again in this session has the
		// same collapse id, and the retract, a delayed silent push, could
		// take that prompt's notification down.
		a.notifUp = false
	}
}

// withdraw ends the card and retracts the notification, whichever is still
// up, with a short budget of its own so a cancelled hook still cleans up.
func (a *ask) withdraw(ctx context.Context) {
	if !a.cardUp && !a.notifUp {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
	defer cancel()
	var wg sync.WaitGroup
	if a.cardUp {
		wg.Go(func() {
			_, err := a.r.Call(ctx, "updateActivity", map[string]string{"slug": a.slug}, nil, map[string]any{"state": "ended"})
			a.r.logf("%s: ended the card: %s", a.slug, describe(err))
		})
	}
	if a.notifUp {
		wg.Go(func() { a.retract(ctx) })
	}
	wg.Wait()
	a.cardUp, a.notifUp = false, false
}

func (a *ask) retract(ctx context.Context) {
	_, err := a.r.Call(ctx, "retractNotifications", nil, nil, map[string]any{"collapse_id": a.slug})
	a.r.logf("%s: retracted the notification: %s", a.slug, describe(err))
	a.notifUp = false
}

// show puts the round on the card: it starts the card, or moves an up card
// to the round. A card the server refuses is not tried again.
func (a *ask) show(ctx context.Context, rd round) bool {
	var err error
	if a.cardUp {
		_, err = a.r.Call(ctx, "updateActivity", map[string]string{"slug": a.slug}, nil, map[string]any{"content": a.content(rd)})
		if isStatus(err, http.StatusNotFound) {
			a.cardUp = false
		}
	} else {
		err = a.start(ctx, rd)
	}
	if err != nil {
		a.r.logf("%s: card refused: %s", a.slug, describe(err))
		a.noCard = true
		return false
	}
	if rd.steps >= 2 {
		a.r.logf("%s: card shows question %d of %d", a.slug, rd.step, rd.steps)
	} else {
		a.r.logf("%s: card shows the prompt", a.slug)
	}
	return true
}

// start creates the card and shows the first round on it.
func (a *ask) start(ctx context.Context, rd round) error {
	params := map[string]string{"slug": a.slug}
	create := map[string]any{
		"slug": a.slug,
		"name": a.agent.Name(),
		// Gone from the Lock Screen as soon as it ends (the server shows an
		// answer for a few seconds first), and deleted a minute later:
		// every prompt has a slug of its own, and ended activities count
		// toward the account's activity limit until they are deleted.
		"dismissal_ttl": 0,
		"ended_ttl":     endedTTL,
	}
	body, err := a.r.Call(ctx, "createActivity", nil, nil, create)
	if err != nil {
		return err
	}
	if hasAnswer(body) {
		// This call was asked before in this session, and re-sending the
		// same options would keep that answer: start the card over.
		if _, err := a.r.Call(ctx, "deleteActivity", params, nil, nil); err != nil {
			return err
		}
		if _, err := a.r.Call(ctx, "createActivity", nil, nil, create); err != nil {
			return err
		}
	}
	if _, err := a.r.Call(ctx, "updateActivity", params, nil, map[string]any{"state": "ongoing", "content": a.content(rd)}); err != nil {
		// The created card would stay on the account (an end schedules its
		// deletion, a create does not), or on the Lock Screen if the update
		// did go through: take it back out.
		a.discard(ctx)
		return err
	}
	a.cardUp, a.cardSince, a.targetsRead = true, time.Now(), false
	return nil
}

// discard deletes the card, which also takes it off the devices, with the
// cleanup budget.
func (a *ask) discard(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
	defer cancel()
	_, err := a.r.Call(ctx, "deleteActivity", map[string]string{"slug": a.slug}, nil, nil)
	a.r.logf("%s: deleted the card: %s", a.slug, describe(err))
}

func hasAnswer(activity []byte) bool {
	var act struct {
		Content struct {
			Answer json.RawMessage `json:"answer"`
		} `json:"content"`
	}
	return json.Unmarshal(activity, &act) == nil && len(act.Content.Answer) > 0 && string(act.Content.Answer) != "null"
}

// content is the approval card for a round. Its options carry no url, so the
// server records the tap.
func (a *ask) content(rd round) map[string]any {
	opts := make([]map[string]any, len(rd.options))
	for i, o := range rd.options {
		m := map[string]any{"id": o.id, "title": o.title}
		if o.style != "" {
			m["style"] = o.style
		}
		if o.icon != "" {
			m["icon"] = o.icon
		}
		if o.unlock {
			m["authentication_required"] = true
		}
		opts[i] = m
	}
	c := map[string]any{
		"template": "approval",
		"state":    rd.question,
		"icon":     "mdi:robot",
		"options":  opts,
		// The deadline, rounded up so the server's own expiry never beats
		// the hook's.
		"end_date":  (a.deadline.UnixNano() + int64(time.Second) - 1) / int64(time.Second),
		"on_expire": "none",
	}
	if src := a.in.repo(); src != "" {
		c["source"] = src
	}
	if len(rd.rows) > 0 {
		c["details"] = rd.rows
	}
	if rd.steps >= 2 {
		c["step"] = rd.step
		c["step_count"] = rd.steps
	}
	return c
}

// startCount is what a start check read: how many of the owner's Apple
// devices the card is meant to reach and how many reported starting it. ok
// is false when the read failed or the server left the counts out.
type startCount struct {
	targeted, started int
	ok                bool
}

// startCounts reads the card's start counts. A transient failure, or a read
// that hangs past startReadWithin, is read again, startReads times in all.
func (a *ask) startCounts(ctx context.Context) startCount {
	for n := 0; ; n++ {
		rctx, cancel := context.WithTimeout(ctx, startReadWithin)
		body, err := a.r.Call(rctx, "getActivity", map[string]string{"slug": a.slug}, nil, nil)
		hung := rctx.Err() != nil
		cancel()
		if err == nil {
			var act struct {
				Targeted *int `json:"devices_targeted"`
				Started  *int `json:"devices_started"`
			}
			if json.Unmarshal(body, &act) != nil || act.Targeted == nil || act.Started == nil {
				return startCount{}
			}
			return startCount{targeted: *act.Targeted, started: *act.Started, ok: true}
		}
		if ctx.Err() != nil {
			return startCount{}
		}
		a.r.logf("%s: reading the card: %s", a.slug, describe(err))
		if n+1 >= startReads || !(api.Transient(err) || hung) {
			return startCount{}
		}
		if api.SleepContext(ctx, api.Backoff(n, err)) != nil {
			return startCount{}
		}
	}
}

// notify sends the round as an actionable notification and returns its id.
// Its actions carry no url, so the server records the tap.
func (a *ask) notify(ctx context.Context, rd round) (string, bool) {
	most := 10
	if rd.other != "" {
		most--
	}
	actions := make([]map[string]any, 0, min(len(rd.options), most)+1)
	for _, o := range rd.options[:min(len(rd.options), most)] {
		// A blank label gets the card's "Option N": the server refuses an
		// action with no title.
		title := cut(oneLine(maskSecrets(o.label)), 64)
		if title == "" {
			title = o.title
		}
		m := map[string]any{"id": o.id, "title": title}
		if o.destructive {
			m["destructive"] = true
		}
		if o.unlock {
			m["authentication_required"] = true
		}
		actions = append(actions, m)
	}
	if rd.other != "" {
		actions = append(actions, map[string]any{"id": rd.other, "title": "Other", "text_input": true, "text_input_placeholder": "Type an answer"})
	}
	n := map[string]any{
		"title":       rd.title,
		"body":        rd.body,
		"level":       "time-sensitive",
		"collapse_id": a.slug,
		"thread_id":   threadID,
		"actions":     actions,
	}
	if repo := a.in.repo(); repo != "" {
		n["subtitle"] = repo
	}
	body, err := a.r.Call(ctx, "createNotification", nil, nil, n)
	if err != nil {
		a.r.logf("%s: notification refused: %s", a.slug, describe(err))
		return "", false
	}
	a.notifUp = true
	var sent struct {
		ID         json.Number `json:"id"`
		Answerable bool        `json:"answerable"`
	}
	if json.Unmarshal(body, &sent) != nil || sent.ID == "" || !sent.Answerable {
		a.r.logf("%s: the notification cannot be answered", a.slug)
		return "", false
	}
	a.r.logf("%s: sent notification %s", a.slug, sent.ID)
	return sent.ID.String(), true
}

// watchCard waits for the card's answer to this round.
func (a *ask) watchCard(ctx context.Context, ids map[string]bool) event {
	gone := func(ended bool) event { return event{from: fromCard, ended: ended} }
	return a.r.wait(ctx, "getActivity", map[string]string{"slug": a.slug}, func(body []byte) (event, bool) {
		var act struct {
			State   string `json:"state"`
			Content struct {
				Answer *struct {
					Option string `json:"option"`
				} `json:"answer"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &act) != nil {
			return gone(false), true
		}
		if ans := act.Content.Answer; ans != nil && ids[ans.Option] {
			return event{from: fromCard, ok: true, reply: reply{option: ans.Option}, ended: act.State == "ended"}, true
		}
		switch act.State {
		case "ended":
			a.r.logf("%s: the card ended with no answer to this question", a.slug)
			return gone(true), true
		case "preempted":
			a.r.logf("%s: the card was preempted", a.slug)
			return gone(false), true
		}
		return event{}, false
	}, func(err error) event { return gone(isStatus(err, http.StatusNotFound)) })
}

// watchNotification waits for the answer to one notification.
func (a *ask) watchNotification(ctx context.Context, id string, ids map[string]bool) event {
	gone := event{from: fromNotification}
	return a.r.wait(ctx, "getNotificationAnswer", map[string]string{"id": id}, func(body []byte) (event, bool) {
		var ans struct {
			Status   string `json:"status"`
			ActionID string `json:"action_id"`
			Text     string `json:"text"`
		}
		if json.Unmarshal(body, &ans) != nil {
			return gone, true
		}
		if ans.Status != "answered" {
			return event{}, false
		}
		if !ids[ans.ActionID] {
			a.r.logf("%s: notification %s was answered with an action this question does not have", a.slug, id)
			return gone, true
		}
		return event{from: fromNotification, ok: true, reply: reply{option: ans.ActionID, text: ans.Text}}, true
	}, func(error) event { return gone })
}

// wait reads op until done returns an event. It long-polls while the account
// has a slot for it and reads every pollEvery once it has none, waits out
// the network and the server, and ends with failed on any other error. It
// returns the zero event when ctx ends.
func (r *Runner) wait(ctx context.Context, op string, params map[string]string, done func([]byte) (event, bool), failed func(error) event) event {
	long := true
	failures := 0
	for {
		var q url.Values
		if long {
			q = url.Values{"wait": {longPollSeconds}}
		}
		start := time.Now()
		body, err := r.Call(ctx, op, params, q, nil)
		switch {
		case ctx.Err() != nil:
			return event{}
		case err == nil:
			failures = 0
			if ev, ok := done(body); ok {
				return ev
			}
		case long && isWaitLimit(err):
			r.logf("%s: every long-poll slot is taken, reading every %s", op, pollEvery)
			long = false
			continue
		case api.Transient(err):
			d := api.Backoff(failures, err)
			failures++
			r.logf("%s: %s, retrying in %s", op, describe(err), d.Round(100*time.Millisecond))
			if api.SleepContext(ctx, d) != nil {
				return event{}
			}
			continue
		default:
			r.logf("%s: %s", op, describe(err))
			return failed(err)
		}
		pause := pollEvery
		if long {
			pause = 0
			if time.Since(start) < quickReturn {
				pause = quickPause
			}
		}
		if pause > 0 && api.SleepContext(ctx, pause) != nil {
			return event{}
		}
	}
}

func isWaitLimit(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.CodeAnswerWaitLimit
}

func isStatus(err error, status int) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Status == status
}

// stepTitle is a notification title with the round's place in a
// multi-question ask.
func stepTitle(title string, step, steps int) string {
	if steps < 2 {
		return title
	}
	return title + " (" + strconv.Itoa(step) + "/" + strconv.Itoa(steps) + ")"
}
