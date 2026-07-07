package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/maikdotfi/metaharness/agent"
)

type ReadFile struct{}

// ReadFileArgs is the tool's input: the path plus an optional line window.
// offset/limit let the model page through a file that's too big to return in
// one shot, the same way it would scroll an editor.
type ReadFileArgs struct {
	Path   string `json:"path" description:"Path of the file to read."`
	Offset int    `json:"offset,omitempty" description:"1-based line number to start reading from. Defaults to the first line."`
	Limit  int    `json:"limit,omitempty" description:"Maximum number of lines to return. Defaults to the read cap noted in the tool description."`
}

var _ agent.TypedTool[ReadFileArgs] = ReadFile{}

const (
	// defaultReadLimit caps how many lines a single read returns when the
	// caller doesn't ask for a specific window, so reading a large file can't
	// flood the model's context. The rest is reachable via offset/limit.
	defaultReadLimit = 2000
	// maxLineLen caps the characters of any single line the read returns. One
	// pathologically long line — a minified bundle, an embedded blob — could
	// otherwise blow the context budget on its own even within the line limit,
	// so it's clipped and the fact is reported back.
	maxLineLen = 2000
)

func (ReadFile) Meta() agent.ToolMeta {
	return agent.ToolMeta{
		Name: "read_file",
		Description: fmt.Sprintf(
			"Read a file in the sandbox. Output is line-numbered in `cat -n` style: a right-aligned line number, a tab, then the line's contents. The line-number prefix is metadata to help you reference lines — it is not part of the file. Long files are truncated to %d lines (use offset and limit to read a specific range), and any single line longer than %d characters is clipped.",
			defaultReadLimit, maxLineLen,
		),
	}
}

func (ReadFile) Execute(ctx context.Context, ec *agent.ExecCtx, args ReadFileArgs) (agent.ToolResult, error) {
	res, err := catFile(ctx, ec, args.Path)
	if err != nil {
		return agent.ToolResult{}, err // infra failure -> fatal
	}
	if res.ExitCode != 0 {
		return agent.ToolResult{Content: execErr(res, "could not read "+args.Path), IsError: true}, nil
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	w := numberLines(res.Stdout, args.Offset, limit)

	switch {
	case w.total == 0:
		// An empty file numbers to nothing; say so rather than returning a
		// blank result the model might misread as an error or a stall.
		return agent.ToolResult{Content: fmt.Sprintf("%s is empty", args.Path)}, nil
	case w.from == 0:
		// offset landed past the last line — report the bound so the model can
		// correct itself instead of silently getting nothing back.
		return agent.ToolResult{Content: fmt.Sprintf("offset %d is past the end of %s (%d lines)", args.Offset, args.Path, w.total)}, nil
	}

	text := w.text
	if w.to < w.total {
		// More follows the returned window; tell the model how to get it.
		text += fmt.Sprintf("\n[showing lines %d-%d of %d; use offset/limit to read more]\n", w.from, w.to, w.total)
	}
	if w.clipped > 0 {
		// Content was altered, not just windowed — flag it so the model knows
		// the lines it sees are incomplete and doesn't trust them for an edit.
		text += fmt.Sprintf("\n[%d line(s) exceeded %d characters and were clipped; their full contents are not shown]\n", w.clipped, maxLineLen)
	}
	return agent.ToolResult{Content: text}, nil
}

// readWindow is the result of rendering a slice of a file: the numbered text,
// the 1-based range shown (from..to) within total lines, and how many of the
// shown lines had their content clipped for exceeding maxLineLen.
type readWindow struct {
	text     string
	from, to int
	total    int
	clipped  int
}

// numberLines renders content in cat -n style: each line prefixed with a
// right-aligned line number and a tab, with over-long lines clipped.
//
// Line counting follows cat: a trailing newline terminates the final line
// rather than starting an empty one, so "a\nb\n" is two lines. from is 0 when
// there is nothing to show — an empty file (total 0) or an offset past EOF.
func numberLines(content string, offset, limit int) readWindow {
	if content == "" {
		return readWindow{}
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total := len(lines)

	from := max(offset, 1)
	if from > total {
		return readWindow{total: total}
	}
	to := total
	if limit > 0 && from+limit-1 < to {
		to = from + limit - 1
	}

	var b strings.Builder
	clipped := 0
	for i := from; i <= to; i++ {
		line, cut := clipLine(lines[i-1])
		if cut {
			clipped++
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	return readWindow{text: b.String(), from: from, to: to, total: total, clipped: clipped}
}

// clipLine truncates s to maxLineLen runes, appending a marker that notes the
// original length. The bool reports whether anything was cut. Truncation is
// rune-aware so it never splits a multi-byte character mid-way.
func clipLine(s string) (string, bool) {
	// Byte length bounds rune count, so a short-enough string can't need
	// clipping and we skip the []rune allocation entirely.
	if len(s) <= maxLineLen {
		return s, false
	}
	r := []rune(s)
	if len(r) <= maxLineLen {
		return s, false
	}
	return string(r[:maxLineLen]) + fmt.Sprintf("… [line clipped: %d characters total]", len(r)), true
}
