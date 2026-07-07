package tools

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
)

type ReadFile struct{}

// ReadFileArgs is the tool's input. The schema sent to the model is derived
// from this type — here, a single required "path".
type ReadFileArgs struct {
	Path string `json:"path" description:"Path of the file to read."`
}

var _ agent.TypedTool[ReadFileArgs] = ReadFile{}

func (ReadFile) Meta() agent.ToolMeta {
	return agent.ToolMeta{
		Name:        "read_file",
		Description: "Read and return the full contents of a file in the sandbox.",
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
	return agent.ToolResult{Content: res.Stdout}, nil
}
