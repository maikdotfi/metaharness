package model

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

// Provider identifies the model API that Config should connect to.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai"
	ProviderGoogle    Provider = "google"
)

// Config contains the provider-specific connection settings needed by New.
// The model ID is selected per agent.Session, so one configured Model can serve
// sessions using different models from the same provider.
type Config struct {
	Provider Provider
	APIKey   string
	BaseURL  string
	Headers  map[string]string
	// Thinking, when non-nil, enables extended thinking (reasoning) output on
	// every request. Leave it nil to keep thinking off.
	Thinking *Thinking
}

// FantasyModel adapts a fantasy.Provider to ModelClient.
// It caches one LanguageModel per model id so switching models within the
// provider is just a different sess.Model string.
type FantasyModel struct {
	provider fantasy.Provider
	mu       sync.Mutex
	cache    map[string]fantasy.LanguageModel

	// thinkingOpts and minOutputTokens hold the extended-thinking settings
	// applied to every request. thinkingOpts is nil when thinking is off.
	thinkingOpts    fantasy.ProviderOptions
	minOutputTokens int64
}

// New builds a ModelClient from metaharness-owned configuration. Callers do
// not need to construct or import a fantasy provider.
func New(cfg Config) (ModelClient, error) {
	cfg.Headers = headersWithAuthorization(cfg.Headers, cfg.APIKey)

	var (
		provider fantasy.Provider
		err      error
	)
	switch cfg.Provider {
	case "", ProviderAnthropic:
		opts := []anthropic.Option{}
		if cfg.APIKey != "" {
			opts = append(opts, anthropic.WithAPIKey(cfg.APIKey))
		}
		if cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, anthropic.WithHeaders(cfg.Headers))
		}
		provider, err = anthropic.New(opts...)
	case ProviderOpenAI:
		opts := []openai.Option{}
		if cfg.APIKey != "" {
			opts = append(opts, openai.WithAPIKey(cfg.APIKey))
		}
		if cfg.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, openai.WithHeaders(cfg.Headers))
		}
		provider, err = openai.New(opts...)
	case ProviderGoogle:
		opts := []google.Option{}
		if cfg.APIKey != "" {
			opts = append(opts, google.WithGeminiAPIKey(cfg.APIKey))
		}
		if cfg.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(cfg.BaseURL))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, google.WithHeaders(cfg.Headers))
		}
		provider, err = google.New(opts...)
	default:
		return nil, fmt.Errorf("unknown model provider %q", cfg.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("building %s model provider: %w", providerName(cfg.Provider), err)
	}

	m := NewFantasyModel(provider)
	if cfg.Thinking != nil {
		opts, minOutput := cfg.Thinking.callOptions(cfg.Provider)
		if opts == nil {
			return nil, fmt.Errorf("thinking is not supported for provider %q", providerName(cfg.Provider))
		}
		m.thinkingOpts = opts
		m.minOutputTokens = minOutput
	}
	return m, nil
}

// This is a bit hacky, but I don't really see a big issue including the API key in the
// Authorization header by default as well. Many providers accept the API key in this header.
func headersWithAuthorization(headers map[string]string, apiKey string) map[string]string {
	if apiKey == "" {
		return headers
	}
	headers = maps.Clone(headers)
	if headers == nil {
		headers = make(map[string]string, 1)
	}
	if _, configured := headers["Authorization"]; !configured {
		headers["Authorization"] = "Bearer " + apiKey
	}
	return headers
}

func providerName(provider Provider) Provider {
	if provider == "" {
		return ProviderAnthropic
	}
	return provider
}

// NewFantasyModel adapts an already constructed fantasy provider. Most callers
// should use New; this constructor remains available for custom providers.
func NewFantasyModel(p fantasy.Provider) *FantasyModel {
	return &FantasyModel{provider: p, cache: map[string]fantasy.LanguageModel{}}
}

func (m *FantasyModel) resolve(ctx context.Context, id string) (fantasy.LanguageModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lm, ok := m.cache[id]; ok {
		return lm, nil
	}
	lm, err := m.provider.LanguageModel(ctx, id)
	if err != nil {
		return nil, err
	}
	m.cache[id] = lm
	return lm, nil
}

func (m *FantasyModel) Generate(ctx context.Context, req ModelRequest) (fantasy.Message, fantasy.Usage, error) {
	lm, err := m.resolve(ctx, req.Model)
	if err != nil {
		return fantasy.Message{}, fantasy.Usage{}, err
	}

	prompt := make([]fantasy.Message, 0, len(req.Messages)+1)
	if req.System != "" {
		prompt = append(prompt, fantasy.NewSystemMessage(req.System))
	}
	prompt = append(prompt, req.Messages...)

	call := fantasy.Call{
		Prompt: fantasy.Prompt(prompt),
		Tools:  toFantasyTools(req.Tools),
	}
	if m.thinkingOpts != nil {
		call.ProviderOptions = m.thinkingOpts
		if m.minOutputTokens > 0 {
			maxOutput := m.minOutputTokens
			call.MaxOutputTokens = &maxOutput
		}
	}

	resp, err := lm.Generate(ctx, call)
	if err != nil {
		return fantasy.Message{}, fantasy.Usage{}, err
	}
	return assistantMessage(resp), resp.Usage, nil
}

func toFantasyTools(defs []ToolDefinition) []fantasy.Tool {
	out := make([]fantasy.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, fantasy.FunctionTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.Schema,
		})
	}
	return out
}

// assistantMessage rebuilds the assistant turn (reasoning + text + tool calls)
// as a fantasy.Message we can append to the transcript and replay next call.
// Reasoning parts come first and carry their provider metadata (e.g. Anthropic
// thinking-block signatures) so a thinking model can validate them on replay.
func assistantMessage(resp *fantasy.Response) fantasy.Message {
	var parts []fantasy.MessagePart
	for _, r := range resp.Content.Reasoning() {
		parts = append(parts, fantasy.ReasoningPart{
			Text:            r.Text,
			ProviderOptions: fantasy.ProviderOptions(r.ProviderMetadata),
		})
	}
	if t := resp.Content.Text(); t != "" {
		parts = append(parts, fantasy.TextPart{Text: t})
	}
	for _, tc := range resp.Content.ToolCalls() {
		parts = append(parts, fantasy.ToolCallPart{
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Input:      tc.Input, // JSON string
		})
	}
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}
