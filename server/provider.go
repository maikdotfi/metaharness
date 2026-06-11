package server

// Provider and model resolution: the one place a metaharness Provider is mapped
// onto a concrete model-library client. Adding a backend is a case in newProvider
// plus a Provider constant in agent.go — nothing else in the codebase changes.

import (
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"
)

// Per-provider defaults, applied when AgentConfig leaves a field empty.
const (
	defaultAnthropicModel = "claude-sonnet-4-5"
	defaultOllamaModel    = "qwen2.5-coder"
	defaultOllamaBaseURL  = "http://localhost:11434/v1"
)

// newProvider maps a metaharness Provider onto a concrete provider client. This
// is the whole switch: adding a backend is a case here plus a Provider constant.
func newProvider(cfg AgentConfig) (fantasy.Provider, error) {
	switch cfg.Provider {
	case ProviderAnthropic, "":
		// API key comes from ANTHROPIC_API_KEY in the environment.
		return anthropic.New()
	case ProviderOllama:
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = defaultOllamaBaseURL
		}
		return openaicompat.New(
			openaicompat.WithName("ollama"),
			openaicompat.WithBaseURL(baseURL),
			// Ollama ignores the key, but the OpenAI client requires a non-empty one.
			openaicompat.WithAPIKey("ollama"),
		)
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// providerName reports the provider a config resolves to, for logging — the
// same defaulting as newProvider.
func providerName(cfg AgentConfig) string {
	if cfg.Provider == "" {
		return string(ProviderAnthropic)
	}
	return string(cfg.Provider)
}

// resolveModel picks the model ID to run, falling back to a per-provider default
// when AgentConfig.Model is empty.
func resolveModel(cfg AgentConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	if cfg.Provider == ProviderOllama {
		return defaultOllamaModel
	}
	return defaultAnthropicModel
}
