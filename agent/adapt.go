package agent

import (
	"context"
	"encoding/json"
	"reflect"

	"charm.land/fantasy/schema"

	"github.com/maikdotfi/metaharness/model"
)

// Adapt erases a TypedTool[T] into the Tool the dispatcher stores. It derives
// the JSON schema from T once, up front, and on every call validates the
// model's arguments against that schema before decoding them into T. The schema
// is the single source of truth: the same T drives both what the model is told
// and what it is held to.
func Adapt[T any](inner TypedTool[T]) Tool {
	return &typedAdapter[T]{
		inner:  inner,
		schema: schema.Generate(reflect.TypeFor[T]()),
	}
}

// AdaptFunc is a shortcut for tools that are just a function — no state, no
// helper methods. It turns a typed handler into a Tool directly, so you skip
// declaring a type and implementing TypedTool. Reach for a TypedTool
// implementation instead when the tool carries dependencies or you want a named
// type to reference and test. This mirrors the http.Handler / http.HandlerFunc
// pairing in the standard library.
func AdaptFunc[T any](
	meta ToolMeta,
	fn func(ctx context.Context, ec *ExecCtx, args T) (ToolResult, error),
) Tool {
	return Adapt(funcTool[T]{meta: meta, fn: fn})
}

type funcTool[T any] struct {
	meta ToolMeta
	fn   func(ctx context.Context, ec *ExecCtx, args T) (ToolResult, error)
}

func (f funcTool[T]) Meta() ToolMeta { return f.meta }

func (f funcTool[T]) Execute(
	ctx context.Context,
	ec *ExecCtx,
	args T,
) (ToolResult, error) {
	return f.fn(ctx, ec, args)
}

type typedAdapter[T any] struct {
	inner  TypedTool[T]
	schema schema.Schema
}

func (a *typedAdapter[T]) Definition() model.ToolDefinition {
	m := a.inner.Meta()
	return model.ToolDefinition{
		Name:        m.Name,
		Description: m.Description,
		Schema:      schema.ToMap(a.schema),
	}
}

func (a *typedAdapter[T]) Execute(
	ctx context.Context,
	ec *ExecCtx,
	input json.RawMessage,
) (ToolResult, error) {
	// Validate against the schema first so missing required fields, bad enums,
	// and out-of-range numbers become a clear message fed back to the model
	// rather than silent Go zero values.
	var obj any
	if err := json.Unmarshal(input, &obj); err != nil {
		return ToolResult{
			Content: "invalid arguments: " + err.Error(),
			IsError: true,
		}, nil
	}
	if err := schema.ValidateAgainstSchema(obj, a.schema); err != nil {
		return ToolResult{
			Content: "invalid arguments: " + err.Error(),
			IsError: true,
		}, nil
	}

	var args T
	if err := json.Unmarshal(input, &args); err != nil {
		return ToolResult{
			Content: "invalid arguments: " + err.Error(),
			IsError: true,
		}, nil
	}
	return a.inner.Execute(ctx, ec, args)
}
