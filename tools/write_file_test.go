package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestWriteFile(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := filepath.Join(dir, "out.txt")
	tool := agent.Adapt(WriteFile{})

	res := testutils.CallTool(t, ec, tool, WriteFileArgs{Path: path, Content: sampleText})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	// The base64 round-trip must preserve the tricky characters exactly.
	if got := testutils.OnDisk(t, path); got != sampleText {
		t.Errorf("file contents = %q, want %q", got, sampleText)
	}
}

// TestWriteFileCreatesParents checks that missing parent directories are created.
func TestWriteFileCreatesParents(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := filepath.Join(dir, "nested", "deep", "out.txt")
	tool := agent.Adapt(WriteFile{})

	res := testutils.CallTool(t, ec, tool, WriteFileArgs{Path: path, Content: "hi"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != "hi" {
		t.Errorf("file contents = %q, want %q", got, "hi")
	}
}

// TestWriteFileOverwrites checks that writing replaces existing content wholesale
// rather than appending — including truncating a longer prior file.
func TestWriteFileOverwrites(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "out.txt", "the old content, which is quite a bit longer")
	tool := agent.Adapt(WriteFile{})

	res := testutils.CallTool(t, ec, tool, WriteFileArgs{Path: path, Content: "new"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != "new" {
		t.Errorf("file contents = %q, want %q", got, "new")
	}
}

// TestWriteFileEmpty checks that empty content produces an empty file (a common
// edge case for the base64 pipeline), not an error.
func TestWriteFileEmpty(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := filepath.Join(dir, "empty.txt")
	tool := agent.Adapt(WriteFile{})

	res := testutils.CallTool(t, ec, tool, WriteFileArgs{Path: path, Content: ""})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != "" {
		t.Errorf("file contents = %q, want empty", got)
	}
}

// TestWriteFileValidatesInput checks the adapter rejects a missing required
// field ("content") before anything reaches the shell.
func TestWriteFileValidatesInput(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	tool := agent.Adapt(WriteFile{})

	input, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "x.txt")})
	res, err := tool.Execute(context.Background(), ec, input)
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error result, got success: %q", res.Content)
	}
}
