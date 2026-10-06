package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Problem codes the client treats specially. The server sends many more;
// everything else is surfaced verbatim.
const (
	CodeQuotaExceeded         = "quota.exceeded"
	CodeAnswerWaitLimit       = "answer_wait.limit_exceeded"
	CodeEncryptionUnavailable = "notification.encryption_unavailable"
)

// Error is a non-2xx response, decoded from the RFC 9457 Problem body the
// API returns on every error.
type Error struct {
	Method string `json:"-"`
	Path   string `json:"-"`
	Status int    `json:"-"`

	Code         string       `json:"code"`
	Title        string       `json:"title"`
	Detail       string       `json:"detail"`
	RetryAfterMs int64        `json:"retry_after_ms"`
	Errors       []FieldError `json:"errors"`

	// Present only on a quota.exceeded 429.
	Kind    string `json:"kind"`
	Used    int    `json:"used"`
	Limit   int    `json:"limit"`
	ResetAt string `json:"reset_at"`
}

// FieldError is one entry of a validation Problem's errors array.
type FieldError struct {
	Message  string `json:"message"`
	Location string `json:"location"`
	Value    any    `json:"value,omitempty"`
}

func newError(method, path string, status int, body []byte) *Error {
	e := &Error{Method: method, Path: path, Status: status}
	if err := json.Unmarshal(body, e); err != nil || (e.Detail == "" && e.Title == "" && e.Code == "") {
		// Not a Problem body (a proxy error page, say). Keep a short excerpt
		// so the user sees something better than a bare status code.
		e.Code, e.Title = "", ""
		e.Detail = excerpt(body)
	}
	return e
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	switch {
	case e.Detail != "":
		fmt.Fprintf(&b, ": %s", e.Detail)
	case e.Title != "":
		fmt.Fprintf(&b, ": %s", e.Title)
	}
	if e.Code == CodeQuotaExceeded && e.Limit > 0 {
		fmt.Fprintf(&b, " (%s %d/%d", e.Kind, e.Used, e.Limit)
		if e.ResetAt != "" {
			fmt.Fprintf(&b, ", resets %s", e.ResetAt)
		}
		b.WriteString(")")
	}
	for _, fe := range e.Errors {
		b.WriteString("\n  ")
		if fe.Location != "" {
			fmt.Fprintf(&b, "%s: ", fe.Location)
		}
		b.WriteString(fe.Message)
		if fe.Value != nil {
			if v, err := json.Marshal(fe.Value); err == nil && len(v) <= 80 {
				fmt.Fprintf(&b, " (got %s)", v)
			}
		}
	}
	return b.String()
}

// TransportError is a request that never produced an HTTP response, after
// any retries the method allowed.
type TransportError struct {
	Method   string
	Path     string
	Attempts int
	Err      error
}

func (e *TransportError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("%s %s: %v (after %d attempts)", e.Method, e.Path, e.Err, e.Attempts)
	}
	return fmt.Sprintf("%s %s: %v", e.Method, e.Path, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
