package model

import (
	"context"

	"charm.land/fantasy"
)

// ToolDefinition is the pure-data tool description sent to the model.
// Mostly added this to avoid `tools` depending directly on `fantasy`
type ToolDefinition struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema
}

// ModelRequest is one stateless completion request.
type ModelRequest struct {
	Model    string
	System   string
	Messages []fantasy.Message
	Tools    []ToolDefinition
}

// ModelClient is the single-completion seam. The fake in tests implements this;
// FantasyModel is the real impl.
type ModelClient interface {
	Generate(ctx context.Context, req ModelRequest) (fantasy.Message, fantasy.Usage, error)
}
