package agent

import "github.com/maikdotfi/metaharness/model"

type Agent struct {
	SystemPrompt string
	Tools        map[string]Tool
	Model        model.ModelClient
	Store        SessionStore
	Newbox       SandboxFactory
}

type Option func(*Agent)

func WithModel(m model.ModelClient) Option { return func(a *Agent) { a.Model = m } }
func WithStore(s SessionStore) Option      { return func(a *Agent) { a.Store = s } }
func WithSandbox(f SandboxFactory) Option  { return func(a *Agent) { a.Newbox = f } }
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
