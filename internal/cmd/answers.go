package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

func newAnswersCmd(a *App) *cobra.Command {
	var after string
	var limit int
	var wait time.Duration
	var follow bool
	c := &cobra.Command{
		Use:   "answers",
		Short: "Read the answers to every notification, after a cursor",
		Long: `Read the answers recorded after the cursor --after, oldest first, across
every notification the key sent. Each response carries the cursor to pass
as --after next time.

Without --after it prints no answers, only the current cursor, so a new
reader does not replay the answers kept for 30 days. To catch every answer
to a notification, start from the answer_cursor its send returned:

  cursor=$(pushward notify --title "Deploy?" --body v2.4.0 --action go=Go --jq .answer_cursor)
  pushward notification answers --after "$cursor" --wait 10m

--wait blocks until at least one answer comes in, and exits 7 if none does
in time. --follow keeps reading and prints the answers as they come in,
one page of JSON per line when piped, until it is interrupted; it then
prints the cursor to resume from on stderr:

  pushward notification answers --follow --jq '.items[] | "\(.notification_id) \(.action_id)"'`,
		Args:        checkArgs,
		Annotations: map[string]string{"operation": "listNotificationAnswers"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > 100) {
				return usagef("--limit takes 1 to 100")
			}
			q := url.Values{}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			ctx := cmd.Context()
			switch {
			case follow && wait > 0:
				return usagef("--follow and --wait are mutually exclusive")
			case follow:
				return a.followAnswers(ctx, after, q)
			case wait > 0:
				return a.waitAnswers(ctx, after, q, wait)
			}
			if after != "" {
				q.Set("after", after)
			}
			resp, err := a.call(ctx, "listNotificationAnswers", nil, q, nil)
			if err != nil {
				return err
			}
			return a.out().Print(resp.Body, func(w io.Writer) error { return writeAnswerPage(w, resp.Body, true) })
		},
	}
	c.Flags().StringVar(&after, "after", "", "cursor of the previous page, or a notification's answer_cursor")
	c.Flags().IntVar(&limit, "limit", 0, "most answers per page, 1 to 100 (server default 100)")
	c.Flags().DurationVar(&wait, "wait", 0, "wait this long for an answer after the cursor")
	c.Flags().BoolVar(&follow, "follow", false, "keep printing answers as they come in, until interrupted")
	return c
}

// answerPage is one response of the answer stream.
type answerPage struct {
	Items  []map[string]any `json:"items"`
	Cursor string           `json:"cursor"`
}

func decodeAnswerPage(body []byte) (answerPage, error) {
	var p answerPage
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("decoding answers: %w", err)
	}
	return p, nil
}

// writeAnswerPage renders a page as one line per answer, with the cursor
// after them when withCursor is set.
func writeAnswerPage(w io.Writer, body []byte, withCursor bool) error {
	p, err := decodeAnswerPage(body)
	if err != nil {
		return err
	}
	for _, it := range p.Items {
		line := fmt.Sprintf("notification %s: %s", str(it, "notification_id"), str(it, "action_id"))
		if t := str(it, "text"); t != "" {
			line += fmt.Sprintf(" (%q)", t)
		}
		if id := str(it, "collapse_id"); id != "" {
			line += ", collapse id " + id
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	if !withCursor {
		return nil
	}
	if len(p.Items) == 0 {
		_, err = fmt.Fprintf(w, "no new answers; cursor %s\n", p.Cursor)
		return err
	}
	_, err = fmt.Fprintf(w, "cursor %s\n", p.Cursor)
	return err
}

// answerCursor reads the stream's current cursor: a call without after
// returns no answers, only that.
func (a *App) answerCursor(ctx context.Context) (string, error) {
	resp, err := a.call(ctx, "listNotificationAnswers", nil, nil, nil)
	if err != nil {
		return "", err
	}
	p, err := decodeAnswerPage(resp.Body)
	if err == nil && p.Cursor == "" {
		err = errors.New("the answer stream returned no cursor")
	}
	return p.Cursor, err
}

// waitAnswers long-polls the stream until at least one answer comes in after
// the cursor, starting from the current one when after is empty.
func (a *App) waitAnswers(ctx context.Context, after string, q url.Values, wait time.Duration) error {
	if after == "" {
		var err error
		if after, err = a.answerCursor(ctx); err != nil {
			return err
		}
	}
	q.Set("after", after)
	last, err := a.poll(ctx, "listNotificationAnswers", nil, q, wait, func(b []byte) (bool, error) {
		p, err := decodeAnswerPage(b)
		return len(p.Items) > 0, err
	})
	if last == nil {
		return err
	}
	return cmp.Or(a.out().Print(last, func(w io.Writer) error { return writeAnswerPage(w, last, true) }), err)
}

// followAnswers reads the stream until interrupted. Each page with answers
// is printed as it arrives: one compact JSON line when piped, so a reader
// can take it line by line. It waits out outages (see followRetries), and
// when it stops the cursor to resume from goes to stderr, unless the cursor
// the caller passed was refused before it read anything.
func (a *App) followAnswers(ctx context.Context, cursor string, q url.Values) (err error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The server handed out the cursor, or a page came back for it.
	confirmed := cursor == ""
	if cursor == "" {
		if cursor, err = a.answerCursor(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	defer func() {
		var ae *api.Error
		refused := !confirmed && errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500
		if !a.quiet && !refused {
			fmt.Fprintf(a.Stderr, "resume with --after %s\n", cursor)
		}
	}()
	q.Set("wait", strconv.Itoa(int(maxServerWait/time.Second)))
	failures := 0
	for {
		q.Set("after", cursor)
		polled := a.Now()
		resp, err := a.call(ctx, "listNotificationAnswers", nil, q, nil)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return nil
		case followRetries(err):
			delay := followBackoff(failures, err)
			failures++
			if !a.quiet {
				a.out().Warnf("%v; retrying in %s", err, delay.Round(100*time.Millisecond))
			}
			if err := a.sleep(ctx, delay); err != nil {
				return nil
			}
			continue
		default:
			return err
		}
		failures = 0
		confirmed = true
		p, err := decodeAnswerPage(resp.Body)
		if err != nil {
			return err
		}
		if len(p.Items) > 0 {
			var line bytes.Buffer
			if err := json.Compact(&line, resp.Body); err != nil {
				return fmt.Errorf("decoding answers: %w", err)
			}
			body := line.Bytes()
			if err := a.out().Print(body, func(w io.Writer) error { return writeAnswerPage(w, body, false) }); err != nil {
				return err
			}
		}
		if p.Cursor != "" {
			cursor = p.Cursor
		}
		// A long-poll that answers at once with nothing (an intermediary
		// dropping the wait) must not turn this into a tight loop.
		if len(p.Items) == 0 && a.Now().Sub(polled) < time.Second {
			if err := a.sleep(ctx, 2*time.Second); err != nil {
				return nil
			}
		}
	}
}

// followRetries reports whether follow waits out err: the network, the
// server and a rate limit (the key's long-poll slots being taken included)
// get better by waiting. Anything else ends it, a key or a cursor the server
// refuses and a used-up quota among them.
func followRetries(err error) bool {
	var te *api.TransportError
	var ae *api.Error
	switch {
	case errors.As(err, &te):
		return true
	case errors.As(err, &ae):
		return ae.Status >= 500 || (ae.Status == http.StatusTooManyRequests && ae.Code != api.CodeQuotaExceeded)
	}
	return false
}

const (
	followRetryMin = 2 * time.Second
	followRetryMax = 30 * time.Second
)

// followBackoff is the wait after the n-th failure in a row, counting from
// 0: 2s doubling to 30s, up to a quarter less for jitter, so readers that
// lost the server together do not come back together. A longer
// retry_after_ms from the server wins, up to the same 30s.
func followBackoff(n int, err error) time.Duration {
	base := min(followRetryMin<<min(n, 5), followRetryMax)
	d := base - rand.N(base/4) // #nosec G404 -- retry jitter, not security-sensitive
	var ae *api.Error
	if errors.As(err, &ae) && ae.RetryAfterMs > 0 {
		d = max(d, min(time.Duration(ae.RetryAfterMs)*time.Millisecond, followRetryMax))
	}
	return d
}
