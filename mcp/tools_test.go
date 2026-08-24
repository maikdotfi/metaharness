package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/mcp"
)

// byName is how a caller finds one reflected tool, and how these tests do.
func byName(ts []agent.Tool, name string) agent.Tool {
	for _, t := range ts {
		if t.Definition().Name == name {
			return t
		}
	}
	return nil
}

func names(ts []agent.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Definition().Name)
	}
	return out
}

// TestAReflectedToolCarriesTheServersOwnSchema is why the reflected door needs
// no validation of its own: the schema the model is shown is the one the server
// published, so a call it accepts is a call the server accepts.
func TestAReflectedToolCarriesTheServersOwnSchema(t *testing.T) {
	srv := mcp.HTTP(echoServer(t))
	defer srv.Close()

	ts, err := srv.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	tool := byName(ts, "echoer_echo")
	if tool == nil {
		t.Fatalf("tools = %v, want echoer_echo among them", names(ts))
	}

	def := tool.Definition()
	if def.Description != "Echo the text back." {
		t.Errorf("Description = %q, want the server's own", def.Description)
	}
	props, ok := def.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Schema = %v, want the server's object schema", def.Schema)
	}
	text, ok := props["text"].(map[string]any)
	if !ok {
		t.Fatalf("Schema properties = %v, want a text property", props)
	}
	if text["description"] != "the text to echo back" {
		t.Errorf("text property = %v, want the server's description verbatim", text)
	}
}

// TestAReflectedToolCallsTheServer closes the loop: the wrapper the model is
// given reaches the wire tool behind the prefixed name.
func TestAReflectedToolCallsTheServer(t *testing.T) {
	srv := mcp.HTTP(echoServer(t))
	defer srv.Close()

	ts, err := srv.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	res, err := byName(ts, "echoer_echo").Execute(
		context.Background(), &agent.ExecCtx{}, json.RawMessage(`{"text":"reflected"}`),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Content != "you said reflected" {
		t.Errorf("Content = %q, want %q", res.Content, "you said reflected")
	}
}

// TestNamesAreSanitizedToWhatProvidersAccept covers a server whose name is not
// spellable as a tool name. Providers require ^[a-zA-Z0-9_-]{1,128}$ and MCP
// does not, so something has to give; replacing the runes keeps the exposed name
// derivable from the server's.
func TestNamesAreSanitizedToWhatProvidersAccept(t *testing.T) {
	endpoint := serve(t, "weird name!", func(srv *sdk.Server) {
		srv.AddTool(&sdk.Tool{Name: "do.thing", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "did it"}}}, nil
			})
	})
	srv := mcp.HTTP(endpoint)
	defer srv.Close()

	ts, err := srv.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if got := names(ts); len(got) != 1 || got[0] != "weird_name__do_thing" {
		t.Fatalf("tools = %v, want [weird_name__do_thing]", got)
	}

	// The sanitized name still has to reach the wire name it was made from.
	res, err := ts[0].Execute(context.Background(), &agent.ExecCtx{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Content != "did it" {
		t.Errorf("Content = %q, want the tool to have run", res.Content)
	}
}

// TestAToolWithNoArgumentsIsCalledWithAnObject matches agent/adapt.go: a model
// asked for a tool that wants nothing sends `{}`, nothing, or a literal null,
// and a server that validates its input must not see the difference.
func TestAToolWithNoArgumentsIsCalledWithAnObject(t *testing.T) {
	endpoint := serve(t, "pinger", func(srv *sdk.Server) {
		sdk.AddTool(srv, &sdk.Tool{Name: "ping"},
			func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil, nil
			})
	})
	srv := mcp.HTTP(endpoint)
	defer srv.Close()

	ts, err := srv.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	tool := byName(ts, "pinger_ping")

	for _, input := range []string{"{}", "", "   ", "null"} {
		t.Run("input "+input, func(t *testing.T) {
			res, err := tool.Execute(context.Background(), &agent.ExecCtx{}, json.RawMessage(input))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.IsError || res.Content != "pong" {
				t.Errorf("result = %+v, want pong", res)
			}
		})
	}
}

// TestATooLongNameIsSkippedAndReported is the one exception to the naming rule,
// and it announces itself: a name mangled to fit would be one the model cannot
// map back to a tool, so it is left out and its siblings still arrive.
func TestATooLongNameIsSkippedAndReported(t *testing.T) {
	huge := strings.Repeat("n", 130)
	endpoint := serve(t, "verbose", func(srv *sdk.Server) {
		srv.AddTool(&sdk.Tool{Name: huge, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{}, nil
			})
		srv.AddTool(&sdk.Tool{Name: "fine", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{}, nil
			})
	})
	rec := &recorder{}
	srv := mcp.HTTP(endpoint, mcp.WithObserver(rec.observe))
	defer srv.Close()

	ts, err := srv.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if got := names(ts); len(got) != 1 || got[0] != "verbose_fine" {
		t.Fatalf("tools = %v, want [verbose_fine]", got)
	}

	skipped := rec.ofType(mcp.EventSkipped)
	if len(skipped) != 1 {
		t.Fatalf("skipped events = %d, want 1", len(skipped))
	}
	if skipped[0].Tool != huge {
		t.Errorf("Tool = %q, want the wire name that was skipped", skipped[0].Tool)
	}
	if skipped[0].Err == nil {
		t.Error("Err is nil, want the reason it was skipped")
	}
}

// TestTheObserverSeesTheProtocolAndTheCatalogue covers what a consumer cannot
// read in their own source: the version that was negotiated, and how many tools
// a server actually turned into.
func TestTheObserverSeesTheProtocolAndTheCatalogue(t *testing.T) {
	rec := &recorder{}
	srv := mcp.HTTP(echoServer(t), mcp.WithObserver(rec.observe))
	defer srv.Close()

	if _, err := srv.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}

	dialed := rec.ofType(mcp.EventDialed)
	if len(dialed) != 1 {
		t.Fatalf("dialed events = %d, want 1", len(dialed))
	}
	if dialed[0].Protocol == "" {
		t.Error("Protocol is empty, want the negotiated version")
	}
	if dialed[0].Server != "echoer" {
		t.Errorf("Server = %q, want the server's own name", dialed[0].Server)
	}

	discovered := rec.ofType(mcp.EventDiscovered)
	if len(discovered) != 1 || discovered[0].Count != 1 {
		t.Errorf("discovered = %+v, want one event counting one tool", discovered)
	}
}

// TestTheObserverSeesAFailedCall is the declared door's only report: a
// hand-written tool naming a wire tool the server does not have shows up here
// and nowhere else.
func TestTheObserverSeesAFailedCall(t *testing.T) {
	rec := &recorder{}
	srv := mcp.HTTP(echoServer(t), mcp.WithObserver(rec.observe))
	defer srv.Close()

	if _, err := srv.Call(context.Background(), "no_such_tool", nil); err == nil {
		t.Log("the server answered rather than erroring; the event still has to name the call")
	}

	called := rec.ofType(mcp.EventCalled)
	if len(called) != 1 {
		t.Fatalf("called events = %d, want 1", len(called))
	}
	if called[0].Tool != "no_such_tool" {
		t.Errorf("Tool = %q, want the tool that was asked for", called[0].Tool)
	}
	if called[0].Duration <= 0 {
		t.Error("Duration is zero, want how long the call took")
	}
}
