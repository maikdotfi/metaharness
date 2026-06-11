package server

// A minimal Model Context Protocol client. MCP over stdio is nothing more than
// newline-delimited JSON-RPC 2.0 spoken to a subprocess, so the standard library
// is the whole client — no SDK, no dependency. This is deliberately in the same
// spirit as Machine.Exec: we just shell out to the server and talk to it.
//
// To use the lightpanda browser as an MCP server, for example, the spec is just:
//
//	MCPServerSpec{Command: "lightpanda", Args: []string{"mcp"}}
//
// This file is model-library-agnostic: it knows nothing about the model library.
// The bridge that adapts these tools into the agent's tools lives in tools.go.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// MCPServerSpec describes how to launch a stdio MCP server: the executable plus
// its arguments, e.g. {Command: "lightpanda", Args: []string{"mcp"}}.
type MCPServerSpec struct {
	Command string
	Args    []string
}

// MCPTool is one tool advertised by an MCP server, in metaharness-neutral form.
// InputSchema is the tool's raw JSON Schema for its arguments.
type MCPTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// mcpServer is a running MCP server we talk to over its stdin/stdout. Calls are
// serialized by mu: the agent's tool loop issues one tool call at a time, which
// is all this needs to handle.
type mcpServer struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	mu     sync.Mutex
	nextID int
}

// startMCP launches the server and completes the MCP initialize handshake.
func startMCP(ctx context.Context, spec MCPServerSpec) (*mcpServer, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Server logs go to stderr; let them through so failures are visible.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", spec.Command, err)
	}

	s := &mcpServer{cmd: cmd, in: in, out: bufio.NewReader(out), nextID: 1}
	if err := s.initialize(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("initializing %q: %w", spec.Command, err)
	}
	return s, nil
}

// initialize performs the required handshake: an initialize request followed by
// an initialized notification, after which tools/list and tools/call are usable.
func (s *mcpServer) initialize(ctx context.Context) error {
	_, err := s.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "metaharness", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	return s.notify("notifications/initialized", nil)
}

// Tools lists the tools the server advertises.
func (s *mcpServer) Tools(ctx context.Context) ([]MCPTool, error) {
	raw, err := s.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	tools := make([]MCPTool, len(res.Tools))
	for i, t := range res.Tools {
		tools[i] = MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return tools, nil
}

// Call invokes a tool by name. arguments is the raw JSON the model produced for
// the tool's input; it is forwarded verbatim. The text content blocks of the
// result are concatenated into a single string.
func (s *mcpServer) Call(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	raw, err := s.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if res.IsError {
		return sb.String(), fmt.Errorf("tool %q reported an error", name)
	}
	return sb.String(), nil
}

// Close shuts the server down.
func (s *mcpServer) Close() error {
	if s.in != nil {
		_ = s.in.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return nil
}

// --- JSON-RPC plumbing ---

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// request writes a JSON-RPC request and reads until the matching response,
// skipping any notifications the server emits in the meantime.
func (s *mcpServer) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextID
	s.nextID++
	if err := s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // not a JSON-RPC message we recognise; ignore
		}
		if resp.ID == nil || *resp.ID != id {
			continue // a notification or an unrelated message
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// notify writes a JSON-RPC notification (a request with no id, no response).
func (s *mcpServer) notify(method string, params any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write marshals one JSON-RPC message and sends it as a single line.
func (s *mcpServer) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing request: %w", err)
	}
	return nil
}
