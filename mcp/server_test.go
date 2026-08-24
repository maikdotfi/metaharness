package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/maikdotfi/metaharness/mcp"
)

var errNoLuck = errors.New("no luck today")

// serve stands up a real SDK server behind httptest, so the tests exercise the
// actual transport rather than a fake of it.
func serve(t *testing.T, name string, add func(*sdk.Server)) string {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: name, Version: "v0.0.1"}, nil)
	add(srv)
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return srv }, nil,
	))
	t.Cleanup(ts.Close)
	return ts.URL
}

type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

// echoServer serves one tool that returns what it was given, which is enough to
// prove arguments went out and text came back.
func echoServer(t *testing.T) string {
	t.Helper()
	return serve(t, "echoer", func(srv *sdk.Server) {
		sdk.AddTool(srv, &sdk.Tool{Name: "echo", Description: "Echo the text back."},
			func(_ context.Context, _ *sdk.CallToolRequest, args echoArgs) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{
					Content: []sdk.Content{&sdk.TextContent{Text: "you said " + args.Text}},
				}, nil, nil
			})
	})
}

// recorder collects what an observer was told, in the order it was told.
type recorder struct {
	mu     sync.Mutex
	events []mcp.Event
}

func (r *recorder) observe(ev mcp.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) all() []mcp.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *recorder) ofType(t mcp.EventType) []mcp.Event {
	var found []mcp.Event
	for _, ev := range r.all() {
		if ev.Type == t {
			found = append(found, ev)
		}
	}
	return found
}

// TestACallRoundTripsArgumentsAndReturnsText is the whole primitive: a server
// value that has done no I/O yet dials on the first call, and the arguments
// reach the tool.
func TestACallRoundTripsArgumentsAndReturnsText(t *testing.T) {
	srv := mcp.HTTP(echoServer(t))
	defer srv.Close()

	res, err := srv.Call(context.Background(), "echo", echoArgs{Text: "hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError = true (%q), want the call to have succeeded", res.Content)
	}
	if res.Content != "you said hello" {
		t.Errorf("Content = %q, want %q", res.Content, "you said hello")
	}
}

// TestAToolThatFailsServerSideIsAnErrorResultNotAnError pins the split that
// matters to the agent loop: the server's own verdict on a tool it ran is a
// result the model reads, while only our failure to reach the server is a Go
// error.
func TestAToolThatFailsServerSideIsAnErrorResultNotAnError(t *testing.T) {
	endpoint := serve(t, "flaky", func(srv *sdk.Server) {
		sdk.AddTool(srv, &sdk.Tool{Name: "boom"},
			func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
				return nil, nil, errNoLuck
			})
	})
	srv := mcp.HTTP(endpoint)
	defer srv.Close()

	res, err := srv.Call(context.Background(), "boom", struct{}{})
	if err != nil {
		t.Fatalf("Call returned a Go error for a tool that ran and failed: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false (%q), want the server's failure passed through", res.Content)
	}
	if res.Content == "" {
		t.Error("Content is empty, want the server's message")
	}
}

// TestStructuredContentWithNoTextArrivesAsJSON covers a tool that answers with
// data rather than prose: the model still needs something to read.
func TestStructuredContentWithNoTextArrivesAsJSON(t *testing.T) {
	endpoint := serve(t, "data", func(srv *sdk.Server) {
		srv.AddTool(&sdk.Tool{Name: "stats", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{
					StructuredContent: map[string]any{"links": 3},
				}, nil
			})
	})
	srv := mcp.HTTP(endpoint)
	defer srv.Close()

	res, err := srv.Call(context.Background(), "stats", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Content), &got); err != nil {
		t.Fatalf("Content is not JSON (%q): %v", res.Content, err)
	}
	if got["links"] != float64(3) {
		t.Errorf("Content = %q, want the structured result", res.Content)
	}
}

// TestACallAfterCloseDialsAgain is what makes `defer srv.Close()` uniformly
// correct: closing releases what is local and nothing more, so a server that is
// used again simply works.
func TestACallAfterCloseDialsAgain(t *testing.T) {
	srv := mcp.HTTP(echoServer(t))
	defer srv.Close()

	if _, err := srv.Call(context.Background(), "echo", echoArgs{Text: "first"}); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := srv.Call(context.Background(), "echo", echoArgs{Text: "second"})
	if err != nil {
		t.Fatalf("Call after Close: %v", err)
	}
	if res.Content != "you said second" {
		t.Errorf("Content = %q, want %q", res.Content, "you said second")
	}
}

// TestAnUnreachableServerFailsTheCall covers the failure that a value-typed
// constructor cannot report: nothing dialed until now, so this is the first
// chance to say the endpoint is not there.
func TestAnUnreachableServerFailsTheCall(t *testing.T) {
	srv := mcp.HTTP("http://127.0.0.1:1/mcp")
	defer srv.Close()

	if _, err := srv.Call(context.Background(), "echo", nil); err == nil {
		t.Error("Call succeeded against an endpoint nobody is serving")
	}
}
