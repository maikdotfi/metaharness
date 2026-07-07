package tools

import (
	"context"
	"fmt"

	"github.com/maikdotfi/metaharness/agent"
)

type WriteFile struct{}

// WriteFileArgs is the tool's input. Both fields are required: writing a file
// always means "put exactly this content here".
type WriteFileArgs struct {
	Path    string `json:"path" description:"Path of the file to write. Parent directories are created as needed."`
	Content string `json:"content" description:"The full contents to write. Any existing file at this path is overwritten."`
}

var _ agent.TypedTool[WriteFileArgs] = WriteFile{}

func (WriteFile) Meta() agent.ToolMeta {
	return agent.ToolMeta{
		Name:        "write_file",
		Description: "Create a file in the sandbox, or overwrite it if it already exists.",
	}
}

func (WriteFile) Execute(ctx context.Context, ec *agent.ExecCtx, args WriteFileArgs) (agent.ToolResult, error) {
	res, err := putFile(ctx, ec, args.Path, args.Content)
	if err != nil {
		return agent.ToolResult{}, err // infra failure -> fatal
	}
	if res.ExitCode != 0 {
		return agent.ToolResult{Content: execErr(res, "could not write "+args.Path), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path)}, nil
}
