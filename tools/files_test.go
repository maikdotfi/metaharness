package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

// This file holds fixtures shared by the read_file, write_file, and edit_file
// tests. The Sandbox they run against is realSandbox from bash_test.go, which
// executes commands through a real shell — so these tests exercise the actual
// cat / base64 plumbing, not a mock.

// sampleText is the shared fixture content. It deliberately packs in characters
// that are awkward to move through a shell — single and double quotes, a dollar
// sign, a backslash, a non-ASCII line, and leading/trailing whitespace — so the
// base64 round-trip in putFile is genuinely stressed rather than getting lucky
// with plain ASCII.
const sampleText = "line one\n" +
	"two's a crowd: \"quoted\", $VAR, back\\slash\n" +
	"café — unicode ☕\n" +
	"  indented tail  \n"

// newExecCtx returns an ExecCtx backed by the real shell, plus a fresh temp
// directory (cleaned up by the testing package) for the test to work in.
func newExecCtx(t *testing.T) (*agent.ExecCtx, string) {
	t.Helper()
	return &agent.ExecCtx{Sandbox: realSandbox{}}, t.TempDir()
}

// seed writes content to dir/name using the OS directly — not the tool under
// test — so a test can arrange a known starting state.
func seed(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("seeding %s: %v", p, err)
	}
	return p
}

// onDisk reads a file straight from disk so a test can assert on a tool's side
// effects independently of the read_file tool.
func onDisk(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// callTool marshals args to JSON and drives the tool through Adapt, mirroring
// the real dispatch path: schema validation, JSON decode, then execution.
func callTool(t *testing.T, ec *agent.ExecCtx, tool agent.Tool, args any) agent.ToolResult {
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
