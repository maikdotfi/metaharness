package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/maikdotfi/metaharness/agent"
)

type EditFile struct{}

// EditFileArgs is the tool's input. The edit is an exact string replacement:
// old_string is matched literally against the file's current contents. By
// default it must match exactly once, which forces the model to supply enough
// surrounding context to make the edit unambiguous; replace_all lifts that.
type EditFileArgs struct {
	Path       string `json:"path" description:"Path of the file to edit."`
	OldString  string `json:"old_string" description:"The exact text to replace, matched literally including whitespace and indentation."`
	NewString  string `json:"new_string" description:"The text to replace old_string with."`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace every occurrence. When false (the default), old_string must occur exactly once."`
}

var _ agent.TypedTool[EditFileArgs] = EditFile{}

func (EditFile) Meta() agent.ToolMeta {
	return agent.ToolMeta{
		Name:        "edit_file",
		Description: "Replace an exact string in an existing file. By default old_string must be unique; set replace_all to change every occurrence.",
	}
}

func (EditFile) Execute(ctx context.Context, ec *agent.ExecCtx, args EditFileArgs) (agent.ToolResult, error) {
	// Guard the degenerate inputs up front so the model gets a precise reason
	// rather than a confusing "not found" or a no-op success.
	if args.OldString == "" {
		return agent.ToolResult{Content: "old_string must not be empty", IsError: true}, nil
	}
	if args.OldString == args.NewString {
		return agent.ToolResult{Content: "old_string and new_string are identical; nothing to change", IsError: true}, nil
	}

	// Read the current contents, do the replacement in Go where matching is
	// exact and testable, then write the whole file back. This sidesteps the
	// escaping hazards of an in-shell sed and keeps the edit logic here.
	res, err := catFile(ctx, ec, args.Path)
	if err != nil {
		return agent.ToolResult{}, err // infra failure -> fatal
	}
	if res.ExitCode != 0 {
		return agent.ToolResult{Content: execErr(res, "could not read "+args.Path), IsError: true}, nil
	}

	content := res.Stdout
	n := strings.Count(content, args.OldString)
	switch {
	case n == 0:
		return agent.ToolResult{Content: fmt.Sprintf("old_string not found in %s", args.Path), IsError: true}, nil
	case n > 1 && !args.ReplaceAll:
		return agent.ToolResult{
			Content: fmt.Sprintf("old_string is not unique in %s (found %d matches); add surrounding context or set replace_all", args.Path, n),
			IsError: true,
		}, nil
	}

	updated := content
	count := 1
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
		count = n
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	wres, err := putFile(ctx, ec, args.Path, updated)
	if err != nil {
		return agent.ToolResult{}, err // infra failure -> fatal
	}
	if wres.ExitCode != 0 {
		return agent.ToolResult{Content: execErr(wres, "could not write "+args.Path), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("made %d replacement(s) in %s", count, args.Path)}, nil
}
