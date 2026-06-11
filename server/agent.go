package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// Agent is the thing metaharness exists to harness. The interface marks the seam
// so the server can orchestrate "an agent" without knowing which one — and,
// crucially, without depending on the model library behind it. Everything below
// this line is metaharness's own vocabulary; the model library is confined to
// the implementation files behind this interface (codingagent.go,
// codereviewagent.go, runner.go, tools.go, provider.go) and never leaks above it.
type Agent interface {
	// Run drives the agent to completion against the given prompt, executing
	// tools inside the supplied machine, and returns the result.
	Run(ctx context.Context, machine Machine, prompt string) (RunResult, error)
}

// AgentKind selects a concrete Agent implementation for the server to run.
type AgentKind string

const (
	// AgentKindCoding is the default general-purpose coding agent.
	AgentKindCoding AgentKind = "coding"
	// AgentKindCodeReview runs the dedicated code review agent.
	AgentKindCodeReview AgentKind = "code-review"
)

// NewAgent builds the concrete agent selected by kind.
func NewAgent(kind AgentKind, cfg AgentConfig) (Agent, error) {
	switch kind {
	case AgentKindCoding, "":
		return NewCodingAgent(cfg), nil
	case AgentKindCodeReview:
		return NewCodeReviewAgent(cfg), nil
	default:
		return nil, fmt.Errorf("unknown agent %q", kind)
	}
}

// RunResult is what an agent hands back after a session: the agent's final
// plain-text answer plus the full transcript of everything that happened.
type RunResult struct {
	// FinalText is the agent's closing message to the user.
	FinalText string
	// Transcript is every message exchanged, in order, ready to persist.
	Transcript []TranscriptMessage
}

// TranscriptMessage is one message in a session transcript. The role is
// metaharness's coarse classification (user | assistant | tool | system); the
// JSON is the model layer's own serialization, kept opaque so we can round-trip
// it without the rest of the codebase knowing the underlying message shape.
type TranscriptMessage struct {
	Role string
	JSON json.RawMessage
}

// Provider selects which model backend an agent runs on. It is metaharness's own
// vocabulary — switching backends never means touching the model library
// directly; the mapping from a Provider to a concrete library client lives
// entirely in provider.go.
type Provider string

const (
	// ProviderAnthropic talks to the Anthropic API. Needs ANTHROPIC_API_KEY.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOllama talks to a local Ollama server over its OpenAI-compatible
	// API. Needs no key; honours BaseURL (default http://localhost:11434/v1).
	ProviderOllama Provider = "ollama"
)

// AgentConfig is everything the creator of an agent chooses. Switching from a
// hosted model to a local one is a one-line change — flip Provider and Model:
//
//	NewCodingAgent(AgentConfig{Provider: ProviderAnthropic, Model: "claude-sonnet-4-5"})
//	NewCodingAgent(AgentConfig{Provider: ProviderOllama, Model: "qwen2.5-coder"})
//	NewCodeReviewAgent(AgentConfig{Provider: ProviderAnthropic, Model: "claude-sonnet-4-5"})
//
// Empty fields fall back to per-provider defaults, so AgentConfig{} is a valid,
// fully-defaulted Anthropic agent.
type AgentConfig struct {
	// Provider is the model backend. Defaults to ProviderAnthropic.
	Provider Provider
	// Model is the model ID for that provider. Empty picks the provider default.
	Model string
	// BaseURL overrides the endpoint for self-hosted providers (Ollama). Ignored
	// by hosted providers. Empty uses the provider default.
	BaseURL string
	// MCPServers are stdio MCP servers for agents that opt into external tools
	// such as the coding agent. The code review agent deliberately ignores
	// these. Example: {Command: "lightpanda", Args: []string{"mcp"}}.
	MCPServers []MCPServerSpec
}
