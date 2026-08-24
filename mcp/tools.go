package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

// maxNameLen is the longest tool name providers accept. MCP itself has no
// limit, so this is the constraint the exposed name has to meet.
const maxNameLen = 128

// Tools lists what the server advertises and wraps each entry as an agent.Tool,
// keeping the server's own description and schema. That is what makes one line
// enough to wire any server.
//
// Nothing is validated locally: the server owns its schema and can answer a bad
// call with a curated error, and a second opinion here would only refuse calls
// the server would have accepted.
//
// Names are exposed as <server>_<wire>, taking the server's own name from the
// handshake. A name that cannot be represented at all is skipped and reported
// through the observer rather than mangled into something the model cannot map
// back to a tool.
func (s *Server) Tools(ctx context.Context) ([]agent.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	sess, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}

	prefix := sanitize(s.serverName())
	var tools []agent.Tool
	for wire, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		exposed, err := exposedName(prefix, wire.Name)
		if err != nil {
			s.report(Event{Type: EventSkipped, Server: s.serverName(), Tool: wire.Name, Err: err})
			continue
		}
		tools = append(tools, &reflected{
			server: s,
			wire:   wire.Name,
			definition: model.ToolDefinition{
				Name:        exposed,
				Description: wire.Description,
				Schema:      schemaMap(wire.InputSchema),
			},
		})
	}

	s.report(Event{Type: EventDiscovered, Server: s.serverName(), Count: len(tools)})
	return tools, nil
}

// reflected is one advertised tool, wrapped. It is the erased agent.Tool
// directly rather than something built with agent.Adapt, because the schema
// comes from the server at runtime and there is no Go type to derive it from.
type reflected struct {
	server     *Server
	wire       string
	definition model.ToolDefinition
}

func (r *reflected) Definition() model.ToolDefinition { return r.definition }

func (r *reflected) Execute(
	ctx context.Context,
	_ *agent.ExecCtx,
	input json.RawMessage,
) (agent.ToolResult, error) {
	// Same leniency as agent/adapt.go: a tool that asks for nothing gets called
	// with nothing, however the model chose to spell it.
	if trimmed := bytes.TrimSpace(input); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		input = json.RawMessage("{}")
	}
	return r.server.Call(ctx, r.wire, input)
}

// exposedName joins the server prefix to a wire name, or refuses. Refusing is
// what keeps the rule honest: every exposed name follows from it, and every
// exception is announced.
func exposedName(prefix, wire string) (string, error) {
	if strings.TrimSpace(wire) == "" {
		return "", fmt.Errorf("tool has no name")
	}
	name := prefix + "_" + sanitize(wire)
	if len(name) > maxNameLen {
		return "", fmt.Errorf("name %q is %d characters, over the %d a tool name may have", name, len(name), maxNameLen)
	}
	return name, nil
}

// sanitize replaces every rune a tool name may not contain, so the exposed name
// stays readable as the one the server published.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// schemaMap is the server's input schema as the model layer wants it. A server
// that publishes no schema still gets one, because a tool definition without it
// is not something a provider accepts.
func schemaMap(raw any) map[string]any {
	switch v := raw.(type) {
	case nil:
		return map[string]any{"type": "object"}
	case map[string]any:
		return v
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return map[string]any{"type": "object"}
		}
		var out map[string]any
		if err := json.Unmarshal(encoded, &out); err != nil {
			return map[string]any{"type": "object"}
		}
		return out
	}
}
