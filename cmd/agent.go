package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maikdotfi/metaharness/server"
)

// newAgentCmd builds the `agent` subcommand for one-shot local runs. This is
// intentionally a thin CLI mode over the same Agent + Machine seam the server
// uses, so demo/eval runs exercise the server agent implementations directly.
func newAgentCmd() *cobra.Command {
	var (
		agent    string
		provider string
		model    string
		baseURL  string
		workdir  string
		prompt   string
		mcp      []string
	)

	cmd := &cobra.Command{
		Use:   "agent [prompt]",
		Short: "Run an agent once against a local workspace",
		Long:  "Run a Meta Harness agent once in CLI mode using a LocalMachine rooted at the selected workspace.",
		Example: strings.TrimSpace(`
metaharness agent --agent code-review --workdir ./demo/code-review --prompt "Review this codebase."
printf 'Review the current workspace.' | metaharness agent --agent code-review --workdir .
`),
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedPrompt, err := agentPrompt(cmd, prompt, args)
			if err != nil {
				return err
			}

			absWorkdir, err := requireDirectory(workdir)
			if err != nil {
				return err
			}

			a, err := server.NewAgent(server.AgentKind(agent), server.AgentConfig{
				Provider:   server.Provider(provider),
				Model:      model,
				BaseURL:    baseURL,
				MCPServers: server.ParseMCPSpecs(mcp),
			})
			if err != nil {
				return err
			}

			result, err := a.Run(cmd.Context(), &server.LocalMachine{Workdir: absWorkdir}, resolvedPrompt)
			if result.FinalText != "" {
				fmt.Fprintln(cmd.OutOrStdout(), result.FinalText)
			}
			return err
		},
	}

	cmd.Flags().StringVar(&agent, "agent", string(server.AgentKindCoding), "agent to run: coding | code-review")
	cmd.Flags().StringVar(&provider, "provider", "anthropic", "model provider: anthropic | ollama")
	cmd.Flags().StringVar(&model, "model", "", "model ID (defaults per provider)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "base URL for self-hosted providers, e.g. Ollama (default http://localhost:11434/v1)")
	cmd.Flags().StringVar(&workdir, "workdir", ".", "workspace directory for the local machine")
	cmd.Flags().StringVar(&prompt, "prompt", "", "prompt to give the agent; omit to use the positional prompt or piped stdin")
	cmd.Flags().StringArrayVar(&mcp, "mcp", nil, `stdio MCP server to expose to agents that use MCP tools, repeatable, e.g. --mcp "lightpanda mcp"`)

	return cmd
}

func agentPrompt(cmd *cobra.Command, flagPrompt string, args []string) (string, error) {
	if flagPrompt != "" && len(args) > 0 {
		return "", fmt.Errorf("pass the prompt either with --prompt, as arguments, or on stdin")
	}
	if flagPrompt != "" {
		return flagPrompt, nil
	}
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}

	stdin := cmd.InOrStdin()
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("missing prompt; pass --prompt, a positional prompt, or pipe stdin")
		}
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("reading prompt from stdin: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("missing prompt; pass --prompt, a positional prompt, or pipe stdin")
	}
	return prompt, nil
}

func requireDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving workdir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("checking workdir %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir %q is not a directory", abs)
	}
	return abs, nil
}
