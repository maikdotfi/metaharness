package model

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// FantasyModel adapts a fantasy.Provider to ModelClient.
// It caches one LanguageModel per model id so switching models within the
// provider is just a different sess.Model string.
type FantasyModel struct {
	provider fantasy.Provider
	mu       sync.Mutex
	cache    map[string]fantasy.LanguageModel
}

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
	prompt = append(prompt, fantasy.NewSystemMessage(req.System))
	prompt = append(prompt, req.Messages...)

	resp, err := lm.Generate(ctx, fantasy.Call{
		Prompt: fantasy.Prompt(prompt),
		Tools:  toFantasyTools(req.Tools),
	})
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

// assistantMessage rebuilds the assistant turn (text + tool calls) as a
// fantasy.Message we can append to the transcript and replay next call.
// Reasoning parts are dropped for simplicity.
func assistantMessage(resp *fantasy.Response) fantasy.Message {
	var parts []fantasy.MessagePart
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
