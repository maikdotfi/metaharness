package cmd

import (
	"github.com/spf13/cobra"

	"github.com/maikdotfi/metaharness/server"
)

// newServeCmd builds the `serve` subcommand.
func newServeCmd() *cobra.Command {
	var (
		addr     string
		agent    string
		provider string
		model    string
		baseURL  string
		mcp      []string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		Long:  "Start the Meta Harness HTTP API server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return server.Run(addr, agent, provider, model, baseURL, mcp)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "address for the HTTP server to listen on")
	cmd.Flags().StringVar(&agent, "agent", string(server.AgentKindCoding), "agent to run: coding | code-review")
	cmd.Flags().StringVar(&provider, "provider", "anthropic", "model provider: anthropic | ollama")
	cmd.Flags().StringVar(&model, "model", "", "model ID (defaults per provider)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "base URL for self-hosted providers, e.g. Ollama (default http://localhost:11434/v1)")
	cmd.Flags().StringArrayVar(&mcp, "mcp", nil, `stdio MCP server to expose as agent tools, repeatable, e.g. --mcp "lightpanda mcp"`)

	return cmd
}
