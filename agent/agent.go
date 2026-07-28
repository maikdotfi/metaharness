package agent

import "github.com/maikdotfi/metaharness/model"

// Agent is the reusable half of a turn: the model, the tools, the prompt and the
// store. It holds nothing per-task and nothing per-sandbox, so one agent serves
// as many sessions at once as there are tasks — each of them bringing the
// sandbox it runs in.
type Agent struct {
	SystemPrompt string
	Tools        map[string]Tool
	Model        model.ModelClient
	Store        SessionStore
}

type Option func(*Agent)

func WithModel(m model.ModelClient) Option { return func(a *Agent) { a.Model = m } }
func WithStore(s SessionStore) Option      { return func(a *Agent) { a.Store = s } }

func WithTools(ts ...Tool) Option {
	return func(a *Agent) {
		a.Tools = make(map[string]Tool, len(ts))
		for _, t := range ts {
			a.Tools[t.Definition().Name] = t
		}
	}
}

func New(systemPrompt string, opts ...Option) *Agent {
	a := &Agent{
		SystemPrompt: systemPrompt,
		Tools:        map[string]Tool{},
		Store:        DiscardStore{},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func (a *Agent) toolDefs() []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0, len(a.Tools))
	for _, t := range a.Tools {
		defs = append(defs, t.Definition())
	}
	return defs
}
