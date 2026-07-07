package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

func TestReadFile(t *testing.T) {
	ec, dir := newExecCtx(t)
	path := seed(t, dir, "hello.txt", sampleText)
	tool := agent.Adapt(ReadFile{})

	res := callTool(t, ec, tool, ReadFileArgs{Path: path})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	// The full contents come back byte-for-byte, special characters intact.
	if res.Content != sampleText {
		t.Errorf("Content = %q, want %q", res.Content, sampleText)
	}
}

func TestReadFileMissing(t *testing.T) {
	ec, dir := newExecCtx(t)
	tool := agent.Adapt(ReadFile{})

	res := callTool(t, ec, tool, ReadFileArgs{Path: filepath.Join(dir, "does-not-exist.txt")})
	if !res.IsError {
		t.Fatalf("expected error for missing file, got success: %q", res.Content)
	}
}

// TestReadFileValidatesInput checks the adapter rejects a missing required
// "path" before anything reaches the shell.
func TestReadFileValidatesInput(t *testing.T) {
	ec, _ := newExecCtx(t)
	tool := agent.Adapt(ReadFile{})

	res, err := tool.Execute(context.Background(), ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error result, got success: %q", res.Content)
	}
}
