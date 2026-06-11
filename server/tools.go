package server

// Tools an agent can call during a run. Tools are defined here, independent of
// any one agent; an agent (see codingagent.go) pulls in whichever ones it wants.
// The model-library type behind a tool is aliased to our own `tool`, so the rest
// of the implementation speaks metaharness's vocabulary and the library stays a
// swappable, low-level detail.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	skillpkg "github.com/maikdotfi/metaharness/skills"
)

// tool is one thing an agent can call. It is an alias for the model library's
// tool type: agents and the runner build and pass `tool`s without naming the
// library directly.
type tool = fantasy.AgentTool

// bashInput is the bash tool's argument schema; the library derives the tool's
// JSON schema from these struct tags.
type bashInput struct {
	Command string `json:"command" description:"The shell command to run inside the machine."`
}

// skillInput is the dedicated Skill tool's argument schema. The model chooses a
// skill by exact catalog name and may pass optional free-form arguments for
// skill-specific context.
type skillInput struct {
	Skill string `json:"skill" description:"The exact name of an available skill to load; do not include a leading slash."`
	Args  string `json:"args,omitempty" description:"Optional arguments or context to pass to the skill."`
}

// bashTool builds the bash tool: the seam where an in-process agent reaches into
// the machine to inspect and modify files. The tool closes over the Machine and
// calls Exec, which is the whole architectural point.
func bashTool(machine Machine) tool {
	return fantasy.NewAgentTool(
		"bash",
		"Run a shell command inside the machine's workspace and return its combined output.",
		func(ctx context.Context, in bashInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			out, exitCode, err := machine.Exec(ctx, in.Command)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to run command: %v", err)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("exit code: %d\n%s", exitCode, out)), nil
		},
	)
}

// skillTool builds the dedicated Skill tool. A skill is not executed out of
// process: invoking the tool loads its instructions into the current agent
// conversation as a tool result, making skill usage visible in transcripts.
func skillTool(available []skillpkg.Skill) tool {
	byName := make(map[string]skillpkg.Skill, len(available))
	for _, s := range available {
		byName[s.Name] = s
	}

	return fantasy.NewAgentTool(
		"Skill",
		skillToolDescription(available),
		func(_ context.Context, in skillInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(in.Skill)
			s, ok := byName[name]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown skill %q; available skills:\n%s", name, skillCatalogList(available))), nil
			}

			var out strings.Builder
			fmt.Fprintf(&out, "<%s>\n", s.Name)
			if args := strings.TrimSpace(in.Args); args != "" {
				fmt.Fprintf(&out, "<args>%s</args>\n\n", args)
			}
			out.WriteString(s.Instructions)
			if !strings.HasSuffix(s.Instructions, "\n") {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, "</%s>", s.Name)
			return fantasy.NewTextResponse(out.String()), nil
		},
	)
}

func skillToolDescription(available []skillpkg.Skill) string {
	return strings.TrimSpace(`Execute a skill within the main conversation.

When users ask for a task that matches an available skill, invoke this tool before generating the substantive response. Use the exact skill name from the catalog, with no leading slash. Never guess or invent skill names.`) + "\n\nAvailable skills:\n" + skillCatalogList(available)
}

func skillCatalogPrompt(available []skillpkg.Skill) string {
	return "Available skills:\n" + skillCatalogList(available) + "\n\nWhen a listed skill matches the task, call the Skill tool before answering. Only use exact skill names from this list."
}

func skillCatalogList(available []skillpkg.Skill) string {
	if len(available) == 0 {
		return "- none"
	}

	var b strings.Builder
	for _, s := range available {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// mcpTools launches every configured MCP server, wraps the tools each advertises
// as `tool`s, and returns them alongside a cleanup func that tears the servers
// down. The servers live for the duration of a run; the caller defers cleanup. On
// any error the already-started servers are closed before returning.
func mcpTools(ctx context.Context, specs []MCPServerSpec) ([]tool, func(), error) {
	var (
		tools   []tool
		servers []*mcpServer
	)
	cleanup := func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}
	for _, spec := range specs {
		server, err := startMCP(ctx, spec)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("mcp %q: %w", spec.Command, err)
		}
		servers = append(servers, server)

		advertised, err := server.Tools(ctx)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("mcp %q list tools: %w", spec.Command, err)
		}
		tools = append(tools, mcpServerTools(server, advertised)...)
	}
	return tools, cleanup, nil
}

// mcpServerTools wraps every tool from one MCP server as a `tool`.
func mcpServerTools(server *mcpServer, tools []MCPTool) []tool {
	out := make([]tool, 0, len(tools))
	for _, t := range tools {
		params, required := splitInputSchema(t.InputSchema)
		out = append(out, &mcpTool{
			server:      server,
			name:        t.Name,
			description: t.Description,
			parameters:  params,
			required:    required,
		})
	}
	return out
}

// mcpTool adapts one MCP tool to the library's tool interface — the bridge the
// library expects you to write yourself, since it has no MCP support of its own.
// The generic constructor can't be used here because an MCP tool's argument
// schema is only known at runtime (it comes from the server), so we implement the
// interface directly and hand the schema over as-is. The model's raw argument
// JSON is forwarded straight to the server, and the server's text result comes
// straight back.
type mcpTool struct {
	server          *mcpServer
	name            string
	description     string
	parameters      map[string]any
	required        []string
	providerOptions fantasy.ProviderOptions
}

func (t *mcpTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        t.name,
		Description: t.description,
		Parameters:  t.parameters,
		Required:    t.required,
	}
}

func (t *mcpTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	out, err := t.server.Call(ctx, t.name, json.RawMessage(call.Input))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("mcp tool %q: %v", t.name, err)), nil
	}
	return fantasy.NewTextResponse(out), nil
}

func (t *mcpTool) ProviderOptions() fantasy.ProviderOptions        { return t.providerOptions }
func (t *mcpTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.providerOptions = opts }

// splitInputSchema pulls the properties map and required list out of an MCP
// tool's JSON Schema, which is exactly the shape the library's ToolInfo wants.
func splitInputSchema(raw json.RawMessage) (map[string]any, []string) {
	var s struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	_ = json.Unmarshal(raw, &s)
	if s.Properties == nil {
		s.Properties = map[string]any{}
	}
	if s.Required == nil {
		s.Required = []string{}
	}
	return s.Properties, s.Required
}
