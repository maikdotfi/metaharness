// Package lightpanda declares the tools worth giving a model from a
// `lightpanda mcp` server.
//
// The reflected door (mcp.Server.Tools) exposes all twenty tools lightpanda
// advertises, which is how a server gets used the day it ships. This is how one
// gets used well: four tools, named and described for the agent rather than for
// the protocol, with arguments the compiler checks. The knobs a model has no
// business choosing — wait strategies, per-call timeouts — are simply not here.
//
// The browser is one browser. Every tool acts on the page the last navigation
// left loaded, so goto then markdown is two calls against one page.
package lightpanda

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/mcp"
)

// Tools returns the browser tools, ready for agent.WithTools.
func Tools(browser *mcp.Server) []agent.Tool {
	return []agent.Tool{
		gotoTool(browser),
		markdown(browser),
		links(browser),
		evaluate(browser),
	}
}

type gotoArgs struct {
	URL string `json:"url" description:"Absolute URL to load, including the scheme."`
}

func gotoTool(browser *mcp.Server) agent.Tool {
	return agent.AdaptFunc(
		agent.ToolMeta{
			Name:        "browser_goto",
			Description: "Load a page in the browser. The page stays loaded for the tools that read it.",
		},
		func(ctx context.Context, _ *agent.ExecCtx, args gotoArgs) (agent.ToolResult, error) {
			return browser.Call(ctx, "goto", args)
		},
	)
}

// pageArgs is shared by the tools that read the loaded page. URL is optional:
// giving one navigates first, which saves a turn when the page is not loaded
// yet.
type pageArgs struct {
	URL string `json:"url,omitempty" description:"Optional URL to load first. Omit to read the page already loaded."`
}

func markdown(browser *mcp.Server) agent.Tool {
	return agent.AdaptFunc(
		agent.ToolMeta{
			Name:        "browser_markdown",
			Description: "Read the loaded page as markdown. This is the tool for reading a page's content.",
		},
		func(ctx context.Context, _ *agent.ExecCtx, args pageArgs) (agent.ToolResult, error) {
			return browser.Call(ctx, "markdown", args)
		},
	)
}

func links(browser *mcp.Server) agent.Tool {
	return agent.AdaptFunc(
		agent.ToolMeta{
			Name:        "browser_links",
			Description: "List the links on the loaded page, to choose where to go next.",
		},
		func(ctx context.Context, _ *agent.ExecCtx, args pageArgs) (agent.ToolResult, error) {
			return browser.Call(ctx, "links", args)
		},
	)
}

type evaluateArgs struct {
	Script string `json:"script" description:"JavaScript evaluated in the page. The value of the last expression is returned."`
	URL    string `json:"url,omitempty" description:"Optional URL to load first. Omit to run against the page already loaded."`
}

func evaluate(browser *mcp.Server) agent.Tool {
	return agent.AdaptFunc(
		agent.ToolMeta{
			Name:        "browser_evaluate",
			Description: "Run JavaScript in the loaded page, for what the other tools do not reach.",
		},
		func(ctx context.Context, _ *agent.ExecCtx, args evaluateArgs) (agent.ToolResult, error) {
			return browser.Call(ctx, "evaluate", args)
		},
	)
}
