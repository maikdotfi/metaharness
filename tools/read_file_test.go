package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestReadFile(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "hello.txt", sampleText)
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	// Every line comes back prefixed with a right-aligned line number and a
	// tab (cat -n style); the content itself, special characters and all, is
	// preserved verbatim after the tab. A short file gets no truncation note.
	want := "     1\tline one\n" +
		"     2\ttwo's a crowd: \"quoted\", $VAR, back\\slash\n" +
		"     3\tcafé — unicode ☕\n" +
		"     4\t  indented tail  \n"
	if res.Content != want {
		t.Errorf("Content = %q, want %q", res.Content, want)
	}
}

// TestReadFileOffsetLimit reads a window out of the middle of a file and
// checks both that only that range is returned and that the model is told how
// much it hasn't seen.
func TestReadFileOffsetLimit(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	path := testutils.Seed(t, dir, "ten.txt", sb.String())
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path, Offset: 3, Limit: 2})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := "     3\tline 3\n" +
		"     4\tline 4\n" +
		"\n[showing lines 3-4 of 10; use offset/limit to read more]\n"
	if res.Content != want {
		t.Errorf("Content = %q, want %q", res.Content, want)
	}
}

// TestReadFileTruncatesLongFiles checks the default cap: a file past
// defaultReadLimit lines returns only the first window plus a note, so a huge
// file can't flood the context.
func TestReadFileTruncatesLongFiles(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	var sb strings.Builder
	for i := 1; i <= defaultReadLimit+500; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	path := testutils.Seed(t, dir, "big.txt", sb.String())
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("\tline %d\n", defaultReadLimit)) {
		t.Errorf("expected last shown line %d in output", defaultReadLimit)
	}
	if strings.Contains(res.Content, fmt.Sprintf("\tline %d\n", defaultReadLimit+1)) {
		t.Errorf("line %d should have been truncated", defaultReadLimit+1)
	}
	wantNote := fmt.Sprintf("[showing lines 1-%d of %d; use offset/limit to read more]", defaultReadLimit, defaultReadLimit+500)
	if !strings.Contains(res.Content, wantNote) {
		t.Errorf("missing truncation note %q in %q", wantNote, res.Content)
	}
}

// TestReadFileClipsLongLines checks that a single over-long line is clipped in
// the output and the read reports that it happened, so the model doesn't treat
// the shown line as complete.
func TestReadFileClipsLongLines(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	content := "short line\n" + strings.Repeat("x", maxLineLen+100) + "\n"
	path := testutils.Seed(t, dir, "wide.txt", content)
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "line clipped") {
		t.Error("expected an inline clip marker on the long line")
	}
	if !strings.Contains(res.Content, "1 line(s) exceeded") {
		t.Errorf("expected a clip notice in output, got %q", res.Content)
	}
	// The short line is untouched; only the long one carries the marker.
	if !strings.Contains(res.Content, "     1\tshort line\n") {
		t.Error("short line should be returned intact")
	}
}

// TestReadFileEmpty checks an empty file reports itself as empty rather than
// returning a blank result.
func TestReadFileEmpty(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "empty.txt", "")
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "is empty") {
		t.Errorf("Content = %q, want an 'is empty' notice", res.Content)
	}
}

// TestReadFileOffsetPastEnd checks an offset beyond the last line reports the
// bound instead of silently returning nothing.
func TestReadFileOffsetPastEnd(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	path := testutils.Seed(t, dir, "hello.txt", sampleText)
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: path, Offset: 99})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "past the end") {
		t.Errorf("Content = %q, want a 'past the end' notice", res.Content)
	}
}

// TestNumberLines exercises the cat-style line counting directly, including the
// trailing-newline and no-trailing-newline edge cases.
func TestNumberLines(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		offset, limit int
		want          readWindow
	}{
		{
			name:    "empty",
			content: "",
			want:    readWindow{},
		},
		{
			name:    "trailing newline is not an extra line",
			content: "a\nb\n",
			want:    readWindow{text: "     1\ta\n     2\tb\n", from: 1, to: 2, total: 2},
		},
		{
			name:    "no trailing newline",
			content: "a\nb",
			want:    readWindow{text: "     1\ta\n     2\tb\n", from: 1, to: 2, total: 2},
		},
		{
			name:    "single empty line",
			content: "\n",
			want:    readWindow{text: "     1\t\n", from: 1, to: 1, total: 1},
		},
		{
			name:    "offset past end yields nothing",
			content: "a\nb\n",
			offset:  5,
			want:    readWindow{total: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := numberLines(tc.content, tc.offset, tc.limit)
			if got != tc.want {
				t.Errorf("numberLines = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestClipLine checks the per-line cap: short lines pass through untouched,
// over-long lines are cut to maxLineLen runes and marked, and the cut lands on
// a rune boundary rather than splitting a multi-byte character.
func TestClipLine(t *testing.T) {
	short := "hello"
	if got, cut := clipLine(short); cut || got != short {
		t.Errorf("clipLine(short) = %q,%v; want unchanged", got, cut)
	}

	// A line of multi-byte runes longer than the cap. Using a multi-byte rune
	// makes byte length exceed the cap well before rune count, exercising both
	// the fast-path guard and the rune-aware cut.
	long := strings.Repeat("é", maxLineLen+50)
	got, cut := clipLine(long)
	if !cut {
		t.Fatal("clipLine(long) reported no cut")
	}
	if !utf8.ValidString(got) {
		t.Error("clipped line is not valid UTF-8 — cut split a rune")
	}
	if !strings.Contains(got, "line clipped") {
		t.Errorf("clipped line missing marker: %q", got[len(got)-40:])
	}
	prefix := string([]rune(long)[:maxLineLen])
	if !strings.HasPrefix(got, prefix) {
		t.Error("clipped line does not start with the first maxLineLen runes")
	}
}

func TestReadFileMissing(t *testing.T) {
	ec, dir := testutils.NewExecCtx(t)
	tool := agent.Adapt(ReadFile{})

	res := testutils.CallTool(t, ec, tool, ReadFileArgs{Path: filepath.Join(dir, "does-not-exist.txt")})
	if !res.IsError {
		t.Fatalf("expected error for missing file, got success: %q", res.Content)
	}
}

// TestReadFileValidatesInput checks the adapter rejects a missing required
// "path" before anything reaches the shell.
func TestReadFileValidatesInput(t *testing.T) {
	ec, _ := testutils.NewExecCtx(t)
	tool := agent.Adapt(ReadFile{})

	res, err := tool.Execute(context.Background(), ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error result, got success: %q", res.Content)
	}
}
