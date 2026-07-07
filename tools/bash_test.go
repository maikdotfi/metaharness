package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestBashExecute(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		want    string
		isError bool
	}{
		{name: "echo", cmd: "echo hello", want: "hello\n"},
		{name: "exit code", cmd: "exit 3", isError: true},
		// Bashisms: these fail under a POSIX /bin/sh, so they prove we run bash.
		{name: "brace expansion", cmd: "echo {1..3}", want: "1 2 3\n"},
		{name: "double brackets", cmd: `[[ foo == f* ]] && echo yes`, want: "yes\n"},
		{name: "here string", cmd: "cat <<< 'hi'", want: "hi\n"},
	}

	// Drive the tool through Adapt so the test exercises the real path:
	// schema validation, JSON decode, then execution against a real shell.
	tool := agent.Adapt(Bash{})
	ec := &agent.ExecCtx{Sandbox: testutils.RealSandbox{}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := testutils.CallTool(t, ec, tool, map[string]string{"cmd": tt.cmd})
			if res.IsError != tt.isError {
				t.Errorf("IsError = %v, want %v (content: %q)", res.IsError, tt.isError, res.Content)
			}
			if tt.want != "" && res.Content != tt.want {
				t.Errorf("Content = %q, want %q", res.Content, tt.want)
			}
		})
	}
}

// TestBashValidatesInput checks that the adapter rejects args that violate the
// schema derived from BashArgs (here: the required "cmd" field is missing)
// before anything reaches the shell.
func TestBashValidatesInput(t *testing.T) {
	tool := agent.Adapt(Bash{})
	ec := &agent.ExecCtx{Sandbox: testutils.RealSandbox{}}

	res, err := tool.Execute(context.Background(), ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error result, got success: %q", res.Content)
	}
}

// TestAdaptFunc checks the function-based authoring path: a bare typed handler
// wrapped by AdaptFunc gets the same schema derivation, validation, and decode
// as an interface tool.
func TestAdaptFunc(t *testing.T) {
	echo := agent.AdaptFunc(
		agent.ToolMeta{Name: "echo", Description: "Echo the message back."},
		func(ctx context.Context, ec *agent.ExecCtx, args struct {
			Msg string `json:"msg"`
		}) (agent.ToolResult, error) {
			return agent.ToolResult{Content: args.Msg}, nil
		},
	)
	ec := &agent.ExecCtx{Sandbox: testutils.RealSandbox{}}

	res, err := echo.Execute(context.Background(), ec, json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "hi" {
		t.Errorf("Content = %q, want %q", res.Content, "hi")
	}

	// Same validation applies: required "msg" missing -> error, handler never runs.
	res, _ = echo.Execute(context.Background(), ec, json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("expected validation error for missing required field, got: %q", res.Content)
	}
}
