package lightpanda_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/mcp"
	"github.com/maikdotfi/metaharness/mcp/lightpanda"
)

// fakeBrowser serves the wire tools lightpanda serves, each answering with its
// own name and the arguments it was given. That is enough to prove a declared
// tool reaches the wire tool it names, without a browser anywhere.
func fakeBrowser(t *testing.T) *mcp.Server {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "lightpanda", Version: "test"}, nil)
	for _, wire := range []string{"goto", "markdown", "links", "evaluate"} {
		name := wire
		srv.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{
					&sdk.TextContent{Text: name + " " + string(req.Params.Arguments)},
				}}, nil
			})
	}
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return srv }, nil,
	))
	t.Cleanup(ts.Close)

	browser := mcp.HTTP(ts.URL)
	t.Cleanup(func() { browser.Close() })
	return browser
}

func tool(t *testing.T, ts []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, candidate := range ts {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

// TestTheDeclaredToolsAreACuratedSubset is the point of the declaring door: four
// tools with names and descriptions we chose, not the twenty the server happens
// to advertise.
func TestTheDeclaredToolsAreACuratedSubset(t *testing.T) {
	ts := lightpanda.Tools(fakeBrowser(t))

	var got []string
	for _, tool := range ts {
		def := tool.Definition()
		got = append(got, def.Name)
		if def.Description == "" {
			t.Errorf("%s has no description", def.Name)
		}
	}
	slices.Sort(got)

	want := []string{"browser_evaluate", "browser_goto", "browser_links", "browser_markdown"}
	if !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// TestTheCompilerCheckedSchemaIsWhatTheModelIsHeldTo covers the other half of
// declaring: the arguments are a Go type, so the schema the model is validated
// against comes from the same place the call does.
func TestTheCompilerCheckedSchemaIsWhatTheModelIsHeldTo(t *testing.T) {
	ts := lightpanda.Tools(fakeBrowser(t))

	for _, tc := range []struct{ tool, required string }{
		{"browser_goto", "url"},
		{"browser_evaluate", "script"},
	} {
		schema := tool(t, ts, tc.tool).Definition().Schema
		required, _ := schema["required"].([]string)
		if !slices.Contains(required, tc.required) {
			t.Errorf("%s requires %v, want %s among them", tc.tool, schema["required"], tc.required)
		}
	}

	res, err := tool(t, ts, "browser_goto").Execute(
		context.Background(), &agent.ExecCtx{}, json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Errorf("goto with no url returned %q, want the missing argument refused", res.Content)
	}
}

// TestEachDeclaredToolReachesItsWireTool is what would otherwise only show up at
// runtime, on a server that renamed something.
func TestEachDeclaredToolReachesItsWireTool(t *testing.T) {
	ts := lightpanda.Tools(fakeBrowser(t))

	for _, tc := range []struct{ tool, args, want string }{
		{"browser_goto", `{"url":"http://example.com"}`, `goto {"url":"http://example.com"}`},
		{"browser_markdown", `{}`, `markdown {}`},
		{"browser_links", `{}`, `links {}`},
		{"browser_evaluate", `{"script":"1+1"}`, `evaluate {"script":"1+1"}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res, err := tool(t, ts, tc.tool).Execute(
				context.Background(), &agent.ExecCtx{}, json.RawMessage(tc.args),
			)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Content != tc.want {
				t.Errorf("Content = %q, want %q", res.Content, tc.want)
			}
		})
	}
}
