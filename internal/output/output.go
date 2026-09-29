// Package output renders API responses: a short human form on a terminal,
// the raw JSON everywhere else, so scripts never parse prose.
package output

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/itchyny/gojq"
)

type Printer struct {
	Out   io.Writer
	Err   io.Writer
	TTY   bool
	JSON  bool
	JQ    string
	Quiet bool

	jq *gojq.Code
}

// Compile checks the --jq expression. Commands call it before their first
// request, so a typo fails before a notification goes out.
func (p *Printer) Compile() error {
	if p.JQ == "" || p.jq != nil {
		return nil
	}
	q, err := gojq.Parse(p.JQ)
	if err != nil {
		return fmt.Errorf("--jq: %w", err)
	}
	if p.jq, err = gojq.Compile(q); err != nil {
		return fmt.Errorf("--jq: %w", err)
	}
	return nil
}

// human reports whether output is the terminal summary rather than JSON.
func (p *Printer) human() bool {
	return !p.Quiet && !p.JSON && p.JQ == "" && p.TTY
}

// Print writes a response body. human renders the terminal form; when it is
// nil the terminal gets indented JSON instead.
func (p *Printer) Print(body []byte, human func(io.Writer) error) error {
	switch {
	case p.Quiet:
		return nil
	case p.JQ != "":
		return p.runJQ(body)
	case !p.human() || human == nil:
		return p.raw(body)
	}
	return human(p.Out)
}

// Printf prints a response body, or on a terminal one summary line.
func (p *Printer) Printf(body []byte, format string, args ...any) error {
	return p.Print(body, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, format+"\n", args...)
		return err
	})
}

// Items prints a list response, or on a terminal a table with one row per
// item.
func (p *Printer) Items(body []byte, headers []string, row func(item map[string]any) []string) error {
	return p.Print(body, func(w io.Writer) error {
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		rows := make([][]string, 0, len(page.Items))
		for _, it := range page.Items {
			rows = append(rows, row(it))
		}
		return Table(w, headers, rows)
	})
}

// Human prints a line only in terminal mode. Commands with no response body
// (a 204 delete) use it for their confirmation.
func (p *Printer) Human(format string, args ...any) {
	if p.human() {
		fmt.Fprintf(p.Out, format+"\n", args...)
	}
}

func (p *Printer) Warnf(format string, args ...any) {
	fmt.Fprintf(p.Err, "warning: "+format+"\n", args...)
}

func (p *Printer) raw(body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	if p.TTY {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err == nil {
			body = buf.Bytes()
		}
	}
	_, err := fmt.Fprintf(p.Out, "%s\n", body)
	return err
}

func (p *Printer) runJQ(body []byte) error {
	if err := p.Compile(); err != nil {
		return err
	}
	var in any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return fmt.Errorf("--jq: response is not JSON: %w", err)
		}
	}
	w := bufio.NewWriter(p.Out)
	iter := p.jq.Run(in)
	for {
		v, ok := iter.Next()
		if !ok {
			return w.Flush()
		}
		if err, ok := v.(error); ok {
			_ = w.Flush()
			if err, ok := err.(*gojq.HaltError); ok && err.Value() == nil {
				return nil
			}
			return fmt.Errorf("--jq: %w", err)
		}
		if s, ok := v.(string); ok {
			fmt.Fprintln(w, s)
			continue
		}
		out, err := gojq.Marshal(v)
		if err != nil {
			_ = w.Flush()
			return fmt.Errorf("--jq: %w", err)
		}
		fmt.Fprintf(w, "%s\n", out)
	}
}

// Table writes aligned columns. Cells are flattened to one line so a
// multi-line field cannot break the layout.
func Table(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = strings.Join(strings.Fields(c), " ")
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}
