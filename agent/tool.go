package agent

import (
	"context"
	"encoding/json"

	"github.com/maikdotfi/metaharness/model"
)

type ToolResult struct {
	Content string
	IsError bool
}

// ExecCtx is what a tool gets at call time. Sandbox is injected here by Run.
type ExecCtx struct {
	Session *Session
	Sandbox Sandbox
}

// Tool is the erased interface the dispatcher holds in its map[string]Tool.
// Args arrive as raw JSON here; use Adapt to author tools with a typed input
// instead of implementing this directly.
type Tool interface {
	Definition() model.ToolDefinition
	Execute(ctx context.Context, ec *ExecCtx, input json.RawMessage) (ToolResult, error)
}

// ToolMeta is the name and description a typed tool declares. The input schema
// is derived from the tool's argument type by Adapt, so it lives here, not on
// the author.
type ToolMeta struct {
	Name        string
	Description string
}

// TypedTool is what tool authors implement. Args are JSON-decoded and validated
// against the derived schema by Adapt before Execute runs, so tools never touch
// raw JSON.
type TypedTool[T any] interface {
	Meta() ToolMeta
	Execute(ctx context.Context, ec *ExecCtx, args T) (ToolResult, error)
}
