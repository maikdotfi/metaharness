// Command code-review runs a review agent against the sample checkout package
// in ./checkout. The agent gets the standard file and shell tools plus the
// skill tool loaded with the grug-review skill, so the review comes back in
// the grug brained developer's voice.
//
// Run from examples/code-review with ANTHROPIC_API_KEY set:
//
//	go run .
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

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
	modelID := flag.String("model", "gemma4:31b-cloud", "Anthropic model id")
	workdir := flag.String("workdir", "checkout", "directory the agent's tools run in")
	prompt := flag.String("prompt", defaultPrompt, "user prompt to start the review with")
	think := flag.Bool("think", false, "enable extended thinking output")
	effort := flag.String("effort", "medium", "thinking effort level (low, medium, high, xhigh, max); only used with -think")
	flag.Parse()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}
	if err := run(context.Background(), *modelID, *workdir, *prompt, *think, *effort); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, modelID, workdir, prompt string, think bool, effort string) error {
	cfg := model.Config{
		Provider: model.ProviderAnthropic,
		APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		BaseURL:  os.Getenv("ANTHROPIC_API_URL"),
	}
	if think {
		cfg.Thinking = &model.Thinking{Effort: model.Effort(effort)}
	}
	m, err := model.New(cfg)
	if err != nil {
		return err
	}

	a := agent.New(systemPrompt,
		agent.WithModel(m),
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
		Messages: []model.Message{model.NewUserMessage(prompt)},
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

func printAssistant(m *model.Message) {
	for _, reasoning := range model.ReasoningParts(m) {
		if reasoning.Text != "" {
			fmt.Printf("\n💭 %s\n", reasoning.Text)
		}
	}
	for _, text := range model.TextParts(m) {
		if text.Text != "" {
			fmt.Printf("\n%s\n", text.Text)
		}
	}
	for _, call := range model.ToolCalls(m) {
		fmt.Printf("→ %s %s\n", call.Name, clip(call.Input, 120))
	}
}

func printToolResult(m *model.Message) {
	for _, result := range model.ToolResults(m) {
		if result.Error != nil {
			fmt.Printf("← error: %v\n", result.Error)
		} else {
			fmt.Printf("← %s\n", clip(result.Text, 200))
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
