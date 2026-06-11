package server

// The code review agent is a separate Agent implementation: it uses the shared
// runner, the bash tool for inspection, and the dedicated Skill tool to load its
// review style. It deliberately does not load MCP tools.

import (
	"context"

	skillpkg "github.com/maikdotfi/metaharness/skills"
)

type codeReviewAgent struct {
	cfg AgentConfig
}

// NewCodeReviewAgent returns an Agent tuned for code review. It uses the
// grug-review skill through the dedicated Skill tool and ignores MCP servers.
func NewCodeReviewAgent(cfg AgentConfig) Agent {
	return &codeReviewAgent{cfg: cfg}
}

const codeReviewSystemPrompt = `You are a code review agent operating inside a sandboxed machine.
Your bash tool runs shell commands in the machine's workspace; use it to inspect
the repository, diffs, tests, and relevant files before reviewing.

Before writing the review, call the Skill tool with skill "grug-review". This is
a blocking requirement for every review. After the skill is loaded, follow its
instructions for tone and review emphasis.

Return findings first, ordered by severity. Focus on bugs, regressions,
complexity, brittle APIs, missing tests, and maintainability risks. Include
file and line references when the workspace gives you enough context. If you find
no issues, say that clearly and note any residual test gaps or uncertainty.`

// Run assembles the code review agent's tools: bash plus the dedicated Skill
// tool with the grug-review skill. MCP tools are intentionally omitted so this
// agent's behavior is review-skill focused and easy to inspect.
func (c *codeReviewAgent) Run(ctx context.Context, machine Machine, prompt string) (RunResult, error) {
	reviewSkills := []skillpkg.Skill{skillpkg.GrugReview()}
	systemPrompt := codeReviewSystemPrompt + "\n\n" + skillCatalogPrompt(reviewSkills)
	tools := []tool{
		bashTool(machine),
		skillTool(reviewSkills),
	}

	return runAgent(ctx, c.cfg, systemPrompt, prompt, tools)
}
