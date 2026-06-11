package server

// The coding agent: metaharness's default Agent implementation. It is a model
// plus a bash tool that runs commands inside the session's machine, and it is
// deliberately thin — it chooses a system prompt and a set of tools, then hands
// off to the runner. Other agents can be built the same way (a different prompt,
// a different tool set) without touching the runner.
//
// The model library (provider, model and tool-use loop) is confined to the
// implementation behind the Agent interface — this file together with runner.go,
// tools.go and provider.go. Nothing above the Agent seam (see agent.go) ever sees
// it, so the library stays a low-level detail we can swap.

import "context"

// codingAgent is the default Agent: a model plus a bash tool reaching into the
// machine. The library runs the tool-use loop internally; the only
// metaharness-specific wiring is that the bash tool closes over the Machine and
// calls Exec, which is the whole architectural point.
type codingAgent struct {
	cfg AgentConfig
}

// NewCodingAgent returns the default Agent implementation. The cfg picks the
// provider and model; see AgentConfig.
func NewCodingAgent(cfg AgentConfig) Agent {
	return &codingAgent{cfg: cfg}
}

const codingSystemPrompt = `You are a coding agent operating inside a sandboxed machine.
Your bash tool runs shell commands in the machine's workspace; use it to inspect
and modify files. Additional tools may also be available — use whichever fit the
task. When the task is complete, stop and summarise what you did in plain text.`

// Run assembles the coding agent's tools — the built-in bash tool plus every tool
// exposed by any configured MCP server — and drives the agent to completion via
// the runner. The MCP servers live only for this run and are torn down when it
// returns.
func (c *codingAgent) Run(ctx context.Context, machine Machine, prompt string) (RunResult, error) {
	tools := []tool{bashTool(machine)}

	mcp, cleanup, err := mcpTools(ctx, c.cfg.MCPServers)
	if err != nil {
		return RunResult{}, err
	}
	defer cleanup()
	tools = append(tools, mcp...)

	return runAgent(ctx, c.cfg, codingSystemPrompt, prompt, tools)
}
