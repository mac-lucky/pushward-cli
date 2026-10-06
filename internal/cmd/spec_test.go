package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

// TestSpecParity holds the operation table and the command tree to the
// OpenAPI spec: every operation the server publishes needs a command, and
// the table must not drift from the spec's methods and paths. Refresh the
// committed openapi.yaml when the API grows, then add what this reports.
//
// PUSHWARD_SPEC_URL=https://api.pushward.app/openapi.yaml runs it against
// the live spec instead.
func TestSpecParity(t *testing.T) {
	data := loadSpec(t)
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Security    []any  `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	var fromSpec []string
	for path, methods := range spec.Paths {
		for method, op := range methods {
			if op.OperationID == "" {
				continue
			}
			fromSpec = append(fromSpec, fmt.Sprintf("%s %s %s public=%v", op.OperationID, strings.ToUpper(method), path, op.Security == nil))
		}
	}
	var fromTable []string
	for _, op := range api.Operations {
		fromTable = append(fromTable, fmt.Sprintf("%s %s %s public=%v", op.ID, op.Method, op.Path, op.Public))
	}
	slices.Sort(fromSpec)
	slices.Sort(fromTable)
	for _, s := range fromSpec {
		if !slices.Contains(fromTable, s) {
			t.Errorf("spec operation missing from api.Operations: %s", s)
		}
	}
	for _, s := range fromTable {
		if !slices.Contains(fromSpec, s) {
			t.Errorf("api.Operations entry not in the spec: %s", s)
		}
	}

	// A command that picks between operations lists them comma-separated.
	covered := map[string]string{}
	walk(NewRoot(NewApp("test", "", "")), func(c *cobra.Command) {
		if ids := c.Annotations["operation"]; ids != "" {
			for id := range strings.SplitSeq(ids, ",") {
				covered[id] = c.CommandPath()
			}
		}
	})
	known := map[string]bool{}
	for _, op := range api.Operations {
		known[op.ID] = true
		if covered[op.ID] == "" {
			t.Errorf("no command runs %s; add one and annotate it with operation=%s", op.ID, op.ID)
		}
	}
	for id, path := range covered {
		if !known[id] {
			t.Errorf("%s is annotated with unknown operation %s", path, id)
		}
	}
}

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}

func loadSpec(t *testing.T) []byte {
	t.Helper()
	if u := os.Getenv("PUSHWARD_SPEC_URL"); u != "" {
		resp, err := http.Get(u) // #nosec G107 -- the developer chooses the spec URL
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("fetching %s: %d %v", u, resp.StatusCode, err)
		}
		return data
	}
	data, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
