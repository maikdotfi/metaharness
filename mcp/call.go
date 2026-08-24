package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/maikdotfi/metaharness/agent"
)

// redialNote goes with the answer to a call that had to reopen the connection,
// whether that answer is a result or an error. Nothing of ours was lost, but a
// server on the legacy lifecycle may have been keeping something — a browser's
// current page is exactly that — and the model has to be told rather than
// reading a blank page as fact.
const redialNote = "[the connection to this server was reopened; anything it held from earlier calls is gone]"

// Call invokes one tool on the server and returns what it answered. args is
// marshalled as the tool's arguments, so it may be a typed struct, a map, or the
// raw JSON a model produced.
//
// The split between the two return values is what the agent loop needs: a
// server that ran the tool and reports failure comes back as a ToolResult with
// IsError set, for the model to read and correct. Only our failure to reach the
// server at all is a Go error.
//
// Every call is bounded by the server's timeout, and a call that finds the
// connection gone opens a new one and tries once more.
func (s *Server) Call(ctx context.Context, tool string, args any) (agent.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	start := time.Now()
	res, redialed, err := s.callOnce(ctx, tool, args)
	s.report(Event{
		Type:     EventCalled,
		Server:   s.serverName(),
		Tool:     tool,
		Duration: time.Since(start),
		Err:      err,
	})
	if err != nil {
		if redialed {
			err = fmt.Errorf("%s: %w", redialNote, err)
		}
		return agent.ToolResult{}, err
	}

	out := collapse(res)
	if redialed {
		out.Content = redialNote + "\n" + out.Content
	}
	return out, nil
}

// callOnce makes the call, retrying once on a connection that turned out to be
// dead. It reports whether it had to redial, which the caller passes on to the
// model.
func (s *Server) callOnce(ctx context.Context, tool string, args any) (*sdk.CallToolResult, bool, error) {
	params := &sdk.CallToolParams{Name: tool, Arguments: args}

	sess, err := s.connect(ctx)
	if err != nil {
		return nil, false, err
	}
	res, err := sess.CallTool(ctx, params)
	if !transportFailed(ctx, err) {
		return res, false, err
	}

	s.drop(sess)
	s.report(Event{Type: EventRedialed, Server: s.serverName(), Tool: tool, Err: err})

	sess, err = s.connect(ctx)
	if err != nil {
		return nil, true, err
	}
	res, err = sess.CallTool(ctx, params)
	return res, true, err
}

// collapse turns a tool result into the string the model reads. Text parts are
// joined; structured data stands in when there is no text; anything else — an
// image, an embedded resource — becomes a line naming what was dropped, because
// silence would read as an empty answer.
func collapse(res *sdk.CallToolResult) agent.ToolResult {
	var (
		lines   []string
		hasText bool
	)
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			lines = append(lines, text.Text)
			hasText = true
			continue
		}
		lines = append(lines, fmt.Sprintf("[dropped %s content]", contentKind(c)))
	}

	if !hasText && res.StructuredContent != nil {
		encoded, err := json.Marshal(res.StructuredContent)
		if err != nil {
			lines = append(lines, "[structured result could not be encoded: "+err.Error()+"]")
		} else {
			lines = append(lines, string(encoded))
		}
	}

	return agent.ToolResult{Content: strings.Join(lines, "\n"), IsError: res.IsError}
}

func contentKind(c sdk.Content) string {
	switch c.(type) {
	case *sdk.ImageContent:
		return "image"
	case *sdk.AudioContent:
		return "audio"
	case *sdk.ResourceLink:
		return "resource link"
	case *sdk.EmbeddedResource:
		return "embedded resource"
	default:
		return fmt.Sprintf("%T", c)
	}
}
