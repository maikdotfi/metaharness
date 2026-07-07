package tools

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
)

type Bash struct{}

// BashArgs is the tool's input. The schema sent to the model — including the
// required "cmd" field and its description — is derived from this type.
type BashArgs struct {
	Cmd string `json:"cmd" description:"The shell command to run."`
}

var _ agent.TypedTool[BashArgs] = Bash{}

func (Bash) Meta() agent.ToolMeta {
	return agent.ToolMeta{
		Name:        "bash",
		Description: "Run a shell command in the sandbox.",
	}
}

func (Bash) Execute(ctx context.Context, ec *agent.ExecCtx, args BashArgs) (agent.ToolResult, error) {
	res, err := ec.Sandbox.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", args.Cmd}})
	if err != nil {
		return agent.ToolResult{}, err // infra failure -> fatal
	}
	out := res.Stdout
	if res.Stderr != "" {
		out += "\n" + res.Stderr
	}
	return agent.ToolResult{Content: out, IsError: res.ExitCode != 0}, nil
}
