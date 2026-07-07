package testutils

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

// NewExecCtx returns an ExecCtx backed by the real shell, plus a fresh temp
// directory (cleaned up by the testing package) for the test to work in.
func NewExecCtx(t *testing.T) (*agent.ExecCtx, string) {
	t.Helper()
	return &agent.ExecCtx{Sandbox: RealSandbox{}}, t.TempDir()
}

// Seed writes content to dir/name using the OS directly — not the tool under
// test — so a test can arrange a known starting state.
func Seed(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("seeding %s: %v", p, err)
	}
	return p
}

// OnDisk reads a file straight from disk so a test can assert on a tool's side
// effects independently of the read_file tool.
func OnDisk(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// CallTool marshals args to JSON and drives the tool through its Execute path,
// mirroring real dispatch: schema validation, JSON decode, then execution. It
// fails the test on an infrastructure error; a tool-level error is returned in
// the ToolResult for the caller to assert on.
func CallTool(t *testing.T, ec *agent.ExecCtx, tool agent.Tool, args any) agent.ToolResult {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling args: %v", err)
	}
	res, err := tool.Execute(context.Background(), ec, input)
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	return res
}

// RecordingTool is a fake agent.Tool that records the raw inputs it was invoked
// with and returns a canned result. Loop tests use it to assert the agent
// dispatched a tool call, without needing a real implementation or sandbox.
type RecordingTool struct {
	Name   string
	Result agent.ToolResult
	Inputs []json.RawMessage
}

func (t *RecordingTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name,
		Description: "records inputs; test double",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *RecordingTool) Execute(_ context.Context, _ *agent.ExecCtx, input json.RawMessage) (agent.ToolResult, error) {
	t.Inputs = append(t.Inputs, input)
	return t.Result, nil
}
