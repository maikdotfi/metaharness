package server

// The runner: the generic engine that drives a model-backed agent to completion.
// Given a provider/model (from AgentConfig), a system prompt and a set of tools,
// it runs the library's tool-use loop and reconstructs the full transcript into
// metaharness's own RunResult. Every Agent implementation that runs on the model
// library funnels through here; what differs between agents is only their system
// prompt and chosen tools (see codingagent.go).

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

// maxSteps backstops the tool-use loop so a misbehaving model can't loop forever.
const maxSteps = 20

// runAgent resolves the provider/model from cfg, runs the agent loop with the
// given system prompt and tools, and returns the final text plus the transcript.
func runAgent(ctx context.Context, cfg AgentConfig, systemPrompt, prompt string, tools []tool) (RunResult, error) {
	provider, err := newProvider(cfg)
	if err != nil {
		return RunResult{}, fmt.Errorf("provider: %w", err)
	}
	modelID := resolveModel(cfg)
	model, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return RunResult{}, fmt.Errorf("language model %q: %w", modelID, err)
	}

	agent := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(tools...),
		fantasy.WithStopConditions(fantasy.StepCountIs(maxSteps)),
	)

	res, err := agent.Generate(ctx, fantasy.AgentCall{Prompt: prompt})
	if err != nil {
		return RunResult{}, fmt.Errorf("agent generate: %w", err)
	}

	return RunResult{
		FinalText:  res.Response.Content.Text(),
		Transcript: buildTranscript(prompt, res),
	}, nil
}

// buildTranscript reconstructs the full message log from an agent result: the
// user prompt, then each step's messages in order (StepResult.Messages is
// per-step, not cumulative), each translated into our own opaque
// TranscriptMessage.
func buildTranscript(prompt string, res *fantasy.AgentResult) []TranscriptMessage {
	messages := []fantasy.Message{fantasy.NewUserMessage(prompt)}
	for _, step := range res.Steps {
		messages = append(messages, step.Messages...)
	}
	transcript := make([]TranscriptMessage, 0, len(messages))
	for _, msg := range messages {
		raw, err := json.Marshal(msg)
		if err != nil {
			// Skip a message we can't serialize rather than failing the run; the
			// work already happened and the rest of the transcript stands.
			continue
		}
		transcript = append(transcript, TranscriptMessage{
			Role: string(msg.Role),
			JSON: raw,
		})
	}
	return transcript
}
