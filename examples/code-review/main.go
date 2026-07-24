// Command code-review runs a review agent against the sample checkout package
// in ./checkout. The agent gets the standard file and shell tools plus the
// skill tool loaded with the grug-review skill, so the review comes back in
// the grug brained developer's voice.
//
// Run from the repository root with ANTHROPIC_API_KEY set:
//
//	go run ./examples/code-review
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/sandbox"
	"github.com/maikdotfi/metaharness/skills"
	"github.com/maikdotfi/metaharness/tools"
)

const systemPrompt = `You are a code review agent. Review the Go code in the current working directory.

Inspect the code and tests with your file tools, run anything useful (like go test ./...) with bash, and finish with review feedback that references files and line numbers.

Before writing any feedback, load the grug-review skill with the skill tool and follow its instructions for the tone and focus of the review.`

const defaultPrompt = "Review this checkout package. Inspect the code and tests, run anything useful, and return review feedback with file and line references."

func main() {
	modelID := flag.String("model", "claude-sonnet-5", "Anthropic model id")
	workdir := flag.String("workdir", "examples/code-review/checkout", "directory the agent's tools run in")
	prompt := flag.String("prompt", defaultPrompt, "user prompt to start the review with")
	flag.Parse()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}
	if err := run(context.Background(), *modelID, *workdir, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, modelID, workdir, prompt string) error {
	provider, err := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	if err != nil {
		return fmt.Errorf("building anthropic provider: %w", err)
	}

	a := agent.New(systemPrompt,
		agent.WithModel(model.NewFantasyModel(provider)),
		agent.WithStore(agent.DiscardStore{}),
		agent.WithSandbox(sandbox.LocalFactory{Root: workdir}),
		agent.WithTools(
			agent.Adapt(tools.Bash{}),
			agent.Adapt(tools.ReadFile{}),
			agent.Adapt(tools.EditFile{}),
			agent.Adapt(tools.WriteFile{}),
			tools.NewSkill(skills.GrugReview()),
		),
	)

	sess := &agent.Session{
		ID:       fmt.Sprintf("review-%d", time.Now().Unix()),
		Model:    modelID,
		Messages: []fantasy.Message{fantasy.NewUserMessage(prompt)},
		Status:   agent.StatusActive,
	}
	fmt.Printf("session %s\n\n", sess.ID)

	events, err := a.Run(ctx, sess)
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case agent.EventAssistant:
			printAssistant(ev.Message)
		case agent.EventToolResult:
			printToolResult(ev.Message)
		case agent.EventDone:
			fmt.Printf("\n--- done: %d input tokens, %d output tokens ---\n",
				sess.Usage.InputTokens, sess.Usage.OutputTokens)
		case agent.EventError:
			return ev.Err
		}
	}
	return nil
}

func printAssistant(m *fantasy.Message) {
	for _, p := range m.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && t.Text != "" {
			fmt.Printf("\n%s\n", t.Text)
		}
		if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
			fmt.Printf("→ %s %s\n", tc.ToolName, clip(tc.Input, 120))
		}
	}
}

func printToolResult(m *fantasy.Message) {
	for _, p := range m.Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		if !ok {
			continue
		}
		switch out := tr.Output.(type) {
		case fantasy.ToolResultOutputContentText:
			fmt.Printf("← %s\n", clip(out.Text, 200))
		case fantasy.ToolResultOutputContentError:
			fmt.Printf("← error: %v\n", out.Error)
		}
	}
}

// clip keeps event logging one-line-ish.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
