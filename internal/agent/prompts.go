package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxQuestions is the most steps one approval card walks through.
const maxQuestions = 10

const (
	deniedMessage  = "Denied from PushWard"
	timeoutMessage = "No answer from PushWard in time"
)

// permission answers a permission prompt: Claude Code's or Codex's
// PermissionRequest.
func (r *Runner) permission(ctx context.Context, agent Agent, in Input) []byte {
	// AskUserQuestion is answered from PreToolUse; a plan review stays on
	// the terminal.
	if agent == Claude && (in.ToolName == "AskUserQuestion" || in.ToolName == "ExitPlanMode") {
		return nil
	}
	if r.atDesk(ctx) {
		return nil
	}
	sum := in.summarize()
	opts := []option{{id: "allow", title: "Allow", label: "Allow", style: "primary", icon: "mdi:check"}}
	// Claude Code applies the permission update it suggested when the
	// answer is "always"; Codex fails closed on one, so it gets no such
	// option.
	var always json.RawMessage
	if agent == Claude && len(in.Suggestions) > 0 {
		always = in.Suggestions[0]
		opts = append(opts, option{id: "always", title: "Always", label: "Always allow", style: "secondary", icon: "mdi:check-all"})
	}
	opts = append(opts, option{id: "deny", title: "Deny", label: "Deny", style: "destructive", icon: "mdi:close", destructive: true})
	var rows []detail
	if sum.row != nil {
		rows = append(rows, *sum.row)
	}
	if repo := in.repo(); repo != "" {
		rows = append(rows, detail{Label: "Repo", Value: repo})
	}
	rd := round{question: sum.state, title: agent.Name() + " asks", body: sum.state, options: opts, rows: rows}

	replies, out := r.newAsk(agent, in).run(ctx, []round{rd})
	switch {
	case out == answered:
		return permissionDecision(replies[0].option, always)
	case out == timedOut && r.Opts.OnTimeout == "deny":
		return permissionDenied(timeoutMessage)
	}
	return nil
}

// questions answers Claude's AskUserQuestion from PreToolUse: one card walks
// through the questions, one step each.
func (r *Runner) questions(ctx context.Context, in Input) []byte {
	qs, err := in.questions()
	if err != nil || len(qs) == 0 || len(qs) > maxQuestions {
		r.logf("AskUserQuestion with %d questions is left to the terminal", len(qs))
		return nil
	}
	if r.atDesk(ctx) {
		return nil
	}
	for _, q := range qs {
		if q.MultiSelect {
			// How Claude Code wants several answers to one question is
			// not settled, so the terminal asks the whole lot, starting
			// with the first.
			r.logf("a question takes several answers, leaving it to the terminal")
			r.notice(ctx, in, Claude.Name()+" needs you", qs[0].Question, "A question is waiting in the terminal")
			return nil
		}
	}
	answers, out := r.askQuestions(ctx, Claude, in, qs)
	switch {
	case out == answered:
		return questionsAnswered(in.ToolInput, qs, answers)
	case out == timedOut && r.Opts.OnTimeout == "deny":
		return preToolUseDenied(timeoutMessage)
	}
	return nil
}

// askQuestions asks the questions in order and returns the answer to each:
// the full label of the chosen option, or the text typed for Other.
func (r *Runner) askQuestions(ctx context.Context, agent Agent, in Input, qs []question) ([]string, outcome) {
	rounds := questionRounds(agent, in.repo(), qs)
	replies, out := r.newAsk(agent, in).run(ctx, rounds)
	if out != answered {
		return nil, out
	}
	answers := make([]string, len(replies))
	for i, rep := range replies {
		answers[i] = rounds[i].answer(rep)
	}
	return answers, answered
}

func questionRounds(agent Agent, repo string, qs []question) []round {
	steps := 0
	if len(qs) >= 2 {
		steps = len(qs)
	}
	rounds := make([]round, len(qs))
	for i, q := range qs {
		rd := round{
			question: cut(oneLine(q.Question), 256),
			other:    fmt.Sprintf("q%dother", i+1),
			steps:    steps,
		}
		if steps > 0 {
			rd.step = i + 1
		}
		rd.title = stepTitle(agent.Name()+" asks", rd.step, steps)
		body := []string{oneLine(q.Question)}
		for j, o := range q.Options {
			label := oneLine(o.Label)
			if label == "" {
				label = fmt.Sprintf("Option %d", j+1)
			}
			opt := option{id: fmt.Sprintf("q%do%d", i+1, j+1), title: cut(label, 24), label: o.Label}
			if len(q.Options) >= 3 {
				// The card needs an icon on every option once there are
				// three; numbers read in order on its compact tiles.
				opt.icon = fmt.Sprintf("mdi:numeric-%d-circle", j+1)
			}
			rd.options = append(rd.options, opt)
			if d := oneLine(o.Description); d != "" {
				body = append(body, label+": "+d)
			}
		}
		rd.body = cut(strings.Join(body, "\n"), 256)
		if repo != "" {
			header := cut(oneLine(q.Header), 24)
			if header == "" {
				header = "Question"
			}
			rd.rows = []detail{{Label: header, Value: repo}}
		}
		rounds[i] = rd
	}
	return rounds
}

// answer is the text a reply gives the agent.
func (rd round) answer(rep reply) string {
	if rd.other != "" && rep.option == rd.other {
		if strings.TrimSpace(rep.text) == "" {
			return "Other"
		}
		return rep.text
	}
	for _, o := range rd.options {
		if o.id == rep.option {
			return o.label
		}
	}
	return ""
}

// The hook outputs. Claude Code and Codex read the same PermissionRequest
// shape; only Claude Code takes updatedPermissions.

type hookOutput struct {
	HookSpecificOutput any `json:"hookSpecificOutput"`
}

type permissionOutput struct {
	HookEventName string   `json:"hookEventName"`
	Decision      decision `json:"decision"`
}

type decision struct {
	Behavior           string            `json:"behavior"`
	UpdatedPermissions []json.RawMessage `json:"updatedPermissions,omitempty"`
	Message            string            `json:"message,omitempty"`
}

type preToolUseOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             any    `json:"updatedInput,omitempty"`
}

func permissionDecision(option string, always json.RawMessage) []byte {
	switch option {
	case "allow":
		return encode(hookOutput{permissionOutput{"PermissionRequest", decision{Behavior: "allow"}}})
	case "always":
		if len(always) == 0 {
			return nil
		}
		return encode(hookOutput{permissionOutput{"PermissionRequest", decision{Behavior: "allow", UpdatedPermissions: []json.RawMessage{always}}}})
	case "deny":
		return permissionDenied(deniedMessage)
	}
	return nil
}

func permissionDenied(message string) []byte {
	return encode(hookOutput{permissionOutput{"PermissionRequest", decision{Behavior: "deny", Message: message}}})
}

func preToolUseDenied(reason string) []byte {
	return encode(hookOutput{preToolUseOutput{HookEventName: "PreToolUse", PermissionDecision: "deny", PermissionDecisionReason: reason}})
}

// questionsAnswered allows AskUserQuestion with the answers filled in:
// Claude Code shows no dialog then. The tool input goes back as it came,
// questions included, with answers keyed by each question's text.
func questionsAnswered(toolInput json.RawMessage, qs []question, answers []string) []byte {
	input, err := parseObject(toolInput)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(qs))
	for i, q := range qs {
		m[q.Question] = answers[i]
	}
	raw, err := marshal(m)
	if err != nil {
		return nil
	}
	input.set("answers", raw)
	return encode(hookOutput{preToolUseOutput{HookEventName: "PreToolUse", PermissionDecision: "allow", UpdatedInput: input}})
}

// ErrNoAnswer is a test card nobody answered in time.
var ErrNoAnswer = errors.New("no answer in time")

// SampleQuestions are what pushward agent test asks.
var SampleQuestions = []struct {
	Question, Header string
	Options          []string
}{
	{"Which database should the demo use?", "Database", []string{"Postgres", "SQLite", "MySQL"}},
	{"Run the migrations now?", "Migrations", []string{"Yes", "Not yet"}},
	{"How should the release go out?", "Release", []string{"Tag and push", "Draft only", "Skip it"}},
}

// Answer is one question of a test and the answer it got.
type Answer struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Test asks SampleQuestions the way a Claude Code question is asked, and
// returns the answers. cwd names the card's source.
func (r *Runner) Test(ctx context.Context, cwd string) ([]Answer, error) {
	qs := make([]question, len(SampleQuestions))
	for i, s := range SampleQuestions {
		qs[i] = question{Question: s.Question, Header: s.Header}
		for _, o := range s.Options {
			qs[i].Options = append(qs[i].Options, questionOption{Label: o})
		}
	}
	toolInput, err := json.Marshal(map[string]any{"questions": qs})
	if err != nil {
		return nil, err
	}
	// A session of its own each run, so a test never meets the last one's
	// card.
	in := Input{SessionID: fmt.Sprintf("test-%d", time.Now().UnixNano()), Cwd: cwd, Event: "PreToolUse", ToolName: "AskUserQuestion", ToolInput: toolInput}
	answers, out := r.askQuestions(ctx, Claude, in, qs)
	switch out {
	case answered:
		res := make([]Answer, len(qs))
		for i, q := range qs {
			res[i] = Answer{q.Question, answers[i]}
		}
		return res, nil
	case timedOut:
		return nil, ErrNoAnswer
	case cancelled:
		return nil, ctx.Err()
	}
	return nil, errors.New("neither a card nor a notification could be sent")
}
