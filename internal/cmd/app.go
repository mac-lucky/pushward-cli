// Package cmd holds the pushward command tree.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mac-lucky/pushward-cli/internal/api"
	"github.com/mac-lucky/pushward-cli/internal/config"
	"github.com/mac-lucky/pushward-cli/internal/output"
)

// Exit codes. Scripts and the GitHub Action branch on these, so they are
// part of the interface: add new ones, never renumber.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitNotFound    = 4
	ExitRateLimited = 5
	ExitUnavailable = 6
	ExitWaitTimeout = 7
)

// maxServerWait is the longest ?wait= the API holds a long-poll open.
const maxServerWait = 25 * time.Second

// App carries process state shared by every command. Tests swap the streams,
// the HTTP client and the clock.
type App struct {
	Version string
	Commit  string
	Date    string

	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	StdoutTTY bool
	StdinTTY  bool

	HTTP   *http.Client
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
	Getenv func(string) string

	// Body is the last successful API response, which the GitHub Action
	// turns into step outputs.
	Body []byte
	// Status overrides the status output for responses without a body.
	Status string

	jsonOut bool
	jq      string
	quiet   bool
	debug   bool
	apiURL  string
	// token, when set, replaces the configured key (the Action's token input).
	token string

	printer *output.Printer
	cfg     config.Config
	client  *api.Client
}

func NewApp(version, commit, date string) *App {
	return &App{
		Version:   version,
		Commit:    commit,
		Date:      date,
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())), // #nosec G115 -- a file descriptor fits in int
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),  // #nosec G115 -- a file descriptor fits in int
		HTTP:      &http.Client{},
		Now:       time.Now,
		Getenv:    os.Getenv,
	}
}

// Execute runs the CLI and returns the process exit code.
func Execute(app *App, args []string) int {
	root := NewRoot(app)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var reported *reportedError
	if !errors.As(err, &reported) {
		fmt.Fprintf(app.Stderr, "error: %v\n", err)
	}
	return ExitCode(err)
}

// reportedError has already been shown in its own format (the Action's
// ::error annotation), so Execute does not print it again.
type reportedError struct{ err error }

func (e *reportedError) Error() string { return e.err.Error() }
func (e *reportedError) Unwrap() error { return e.err }

// UsageError marks a mistake in how the command was invoked.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func usagef(format string, args ...any) error {
	return &UsageError{fmt.Errorf(format, args...)}
}

// errWaitTimeout is returned when a wait runs out with no answer. The last
// response is still printed.
var errWaitTimeout = errors.New("timed out waiting for an answer")

var errNoToken = errors.New("no API key: set " + config.EnvToken + " or run `pushward auth login`")

func ExitCode(err error) int {
	var ue *UsageError
	var ae *api.Error
	var te *api.TransportError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ue):
		return ExitUsage
	case errors.Is(err, errWaitTimeout):
		return ExitWaitTimeout
	case errors.Is(err, errNoToken):
		return ExitAuth
	case errors.As(err, &ae):
		switch {
		case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
			return ExitAuth
		case ae.Status == http.StatusNotFound:
			return ExitNotFound
		case ae.Status == http.StatusTooManyRequests:
			return ExitRateLimited
		case ae.Status >= 500:
			return ExitUnavailable
		}
	case errors.As(err, &te):
		return ExitUnavailable
	}
	return ExitError
}

func (a *App) out() *output.Printer {
	if a.printer == nil {
		a.printer = &output.Printer{Out: a.Stdout, Err: a.Stderr, TTY: a.StdoutTTY, JSON: a.jsonOut, JQ: a.jq, Quiet: a.quiet}
	}
	return a.printer
}

func (a *App) newClient(baseURL, token string) *api.Client {
	c := &api.Client{
		BaseURL:   baseURL,
		Token:     token,
		UserAgent: fmt.Sprintf("pushward-cli/%s (%s/%s)", a.Version, runtime.GOOS, runtime.GOARCH),
		HTTP:      a.HTTP,
		Sleep:     a.Sleep,
	}
	if a.debug {
		c.Debug = a.Stderr
	}
	return c
}

// api builds the client on first use, so commands that never touch the API
// (version, completion, --help) work without a config. A missing key is
// reported by call, since public operations do not need one.
func (a *App) api() (*api.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	cfg, err := config.Load(a.apiURL)
	if err != nil {
		return nil, err
	}
	if a.token != "" {
		cfg.Token = a.token
	} else if cfg.Warning != "" {
		a.out().Warnf("%s", cfg.Warning)
	}
	a.cfg = cfg
	a.client = a.newClient(cfg.APIURL, cfg.Token)
	return a.client, nil
}

// keyed returns the client, failing when there is no key to send.
func (a *App) keyed() (*api.Client, error) {
	c, err := a.api()
	if err == nil && c.Token == "" {
		err = errNoToken
	}
	return c, err
}

// call runs one operation and records its response for the Action.
func (a *App) call(ctx context.Context, opID string, params map[string]string, query url.Values, body map[string]any) (*api.Response, error) {
	c, err := a.api()
	if err != nil {
		return nil, err
	}
	if c.Token == "" && !api.Op(opID).Public {
		return nil, errNoToken
	}
	var data []byte
	if body != nil {
		if data, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	resp, err := c.Call(ctx, opID, params, query, data)
	if err != nil {
		return nil, err
	}
	a.Body = resp.Body
	return resp, nil
}

func (a *App) sleep(ctx context.Context, d time.Duration) error {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	return api.SleepContext(ctx, d)
}

// poll long-polls an operation until done accepts a response or wait runs
// out, and returns the last response. On timeout the error is
// errWaitTimeout, still with the last response.
func (a *App) poll(ctx context.Context, opID string, params map[string]string, wait time.Duration, done func(body []byte) (bool, error)) ([]byte, error) {
	deadline := a.Now().Add(wait)
	for {
		left := deadline.Sub(a.Now())
		q := url.Values{}
		if left >= time.Second {
			q.Set("wait", strconv.Itoa(int(min(left, maxServerWait)/time.Second)))
		}
		polled := a.Now()
		resp, err := a.call(ctx, opID, params, q, nil)
		var ae *api.Error
		switch {
		case errors.As(err, &ae) && ae.Code == api.CodeAnswerWaitLimit && a.Now().Before(deadline):
			// Every long-poll slot is taken; back off instead of queueing.
			if err := a.sleep(ctx, 5*time.Second); err != nil {
				return nil, err
			}
			continue
		case err != nil:
			return nil, err
		}
		if ok, err := done(resp.Body); ok || err != nil {
			return resp.Body, err
		}
		if !a.Now().Before(deadline) {
			return resp.Body, errWaitTimeout
		}
		// A long-poll that answers at once (an intermediary dropping the
		// wait) must not turn this into a tight loop.
		if a.Now().Sub(polled) < time.Second {
			if err := a.sleep(ctx, 2*time.Second); err != nil {
				return nil, err
			}
		}
	}
}

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

var keyIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// positionals returns the <name> placeholders of a command's Use line, the
// one place a command declares its arguments.
func positionals(c *cobra.Command) []string {
	var names []string
	for _, f := range strings.Fields(c.Use) {
		if strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") {
			names = append(names, f[1:len(f)-1])
		}
	}
	return names
}

// checkArgs is every command's Args: the count comes from the Use line, and
// slug and id values are validated before any request.
func checkArgs(cmd *cobra.Command, got []string) error {
	names := positionals(cmd)
	if len(got) != len(names) {
		if len(names) == 0 {
			return usagef("%s takes no arguments", cmd.CommandPath())
		}
		return usagef("%s needs <%s>", cmd.CommandPath(), strings.Join(names, "> <"))
	}
	for i, name := range names {
		switch name {
		case "slug":
			if !slugPattern.MatchString(got[i]) {
				return usagef("invalid slug %q: use 1-128 letters, digits, - or _, starting with a letter or digit", got[i])
			}
		case "id":
			if _, err := strconv.ParseUint(got[i], 10, 64); err != nil {
				return usagef("invalid id %q: want a number", got[i])
			}
		case "key-id":
			if !keyIDPattern.MatchString(got[i]) {
				return usagef("invalid key id %q: want a UUID from pushward key list", got[i])
			}
		}
	}
	return nil
}

// decode unmarshals a response body into a loose map for the human renderers.
func decode(body []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return m
}

func str(m map[string]any, keys ...string) string {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = mm[k]
	}
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.10g", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}
