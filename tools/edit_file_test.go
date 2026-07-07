package tools

import (
	"strings"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestEditFile(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "code.txt", "alpha beta gamma")
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "beta", NewString: "BETA"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != "alpha BETA gamma" {
		t.Errorf("file contents = %q, want %q", got, "alpha BETA gamma")
	}
}

// TestEditFileNotUnique checks that a non-unique old_string is rejected (without
// replace_all) and the file is left untouched.
func TestEditFileNotUnique(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	const orig = "x marks x"
	path := testutils.Seed(t, dir, "code.txt", orig)
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "x", NewString: "y"})
	if !res.IsError {
		t.Fatalf("expected error for non-unique old_string, got success: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != orig {
		t.Errorf("file was modified on a failed edit: %q", got)
	}
}

// TestEditFileReplaceAll checks that replace_all changes every occurrence.
func TestEditFileReplaceAll(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "code.txt", "x marks x, and x again")
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "x", NewString: "y", ReplaceAll: true})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if got, want := testutils.OnDisk(t, path), "y marks y, and y again"; got != want {
		t.Errorf("file contents = %q, want %q", got, want)
	}
}

// TestEditFileNotFound checks that an old_string absent from the file is an
// error and leaves the file untouched.
func TestEditFileNotFound(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	const orig = "alpha beta gamma"
	path := testutils.Seed(t, dir, "code.txt", orig)
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "delta", NewString: "DELTA"})
	if !res.IsError {
		t.Fatalf("expected error for absent old_string, got success: %q", res.Content)
	}
	if got := testutils.OnDisk(t, path); got != orig {
		t.Errorf("file was modified on a failed edit: %q", got)
	}
}

// TestEditFileMissingFile checks that editing a nonexistent file is an error.
func TestEditFileMissingFile(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: dir + "/nope.txt", OldString: "a", NewString: "b"})
	if !res.IsError {
		t.Fatalf("expected error for missing file, got success: %q", res.Content)
	}
}

// TestEditFileNoop checks that an identical old/new string is rejected rather
// than silently succeeding.
func TestEditFileNoop(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "code.txt", "alpha")
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "alpha", NewString: "alpha"})
	if !res.IsError {
		t.Fatalf("expected error for identical old/new string, got success: %q", res.Content)
	}
}

// TestEditFilePreservesSpecialChars edits a file full of shell-hostile content
// and checks the untouched parts survive the cat/base64 round-trip exactly.
func TestEditFilePreservesSpecialChars(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "code.txt", sampleText)
	tool := agent.Adapt(EditFile{})

	res := testutils.CallTool(t, ec, tool, EditFileArgs{Path: path, OldString: "café", NewString: "tea"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := strings.Replace(sampleText, "café", "tea", 1)
	if got := testutils.OnDisk(t, path); got != want {
		t.Errorf("file contents = %q, want %q", got, want)
	}
}
