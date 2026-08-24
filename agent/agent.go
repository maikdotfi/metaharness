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
	Memory       Memory
}

type Option func(*Agent)

func WithModel(m model.ModelClient) Option { return func(a *Agent) { a.Model = m } }
func WithStore(s SessionStore) Option      { return func(a *Agent) { a.Store = s } }

// WithTools adds tools to the agent. It is additive, so tools that come from
// different places — built-ins here, an MCP server's there — can be passed
// separately without one call discarding another's.
//
// Two tools with the same name is a panic, the way a duplicate sandbox backend
// is: the model would be told about one and reach the other, and that is a
// mistake in the wiring rather than a condition to recover from.
func WithTools(ts ...Tool) Option {
	return func(a *Agent) {
		if a.Tools == nil {
			a.Tools = make(map[string]Tool, len(ts))
		}
		for _, t := range ts {
			name := t.Definition().Name
			if _, taken := a.Tools[name]; taken {
				panic("agent: tool " + name + " is registered twice")
			}
			a.Tools[name] = t
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
