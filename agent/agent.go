package agent

import "github.com/maikdotfi/metaharness/model"

type Agent struct {
	SystemPrompt string
	Tools        map[string]Tool
	Model        model.ModelClient
	Store        SessionStore
	Newbox       SandboxFactory

	// Sandbox is the default spec runs acquire with. A session that already
	// names a sandbox overrides it, which is how a resumed session returns to
	// the sandbox it ran in.
	Sandbox SandboxSpec
}

type Option func(*Agent)

func WithModel(m model.ModelClient) Option { return func(a *Agent) { a.Model = m } }
func WithStore(s SessionStore) Option      { return func(a *Agent) { a.Store = s } }

// WithSandbox supplies the factory: how sandboxes are made.
func WithSandbox(f SandboxFactory) Option { return func(a *Agent) { a.Newbox = f } }

// WithSandboxSpec supplies the default identity: which sandbox runs use. The
// sandbox is an application-lifetime choice, so it belongs on the agent rather
// than on every session.
func WithSandboxSpec(s SandboxSpec) Option { return func(a *Agent) { a.Sandbox = s } }
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

// SandboxFor returns the sandbox spec a run of sess would use: the session's own
// spec when it has one, otherwise the agent's default, otherwise the zero spec
// (which backends without images ignore). Because sandboxes are adopted by name,
// resolving the same spec twice attaches rather than creating a second sandbox.
func (a *Agent) SandboxFor(sess *Session) SandboxSpec {
	if sess != nil && sess.Sandbox != (SandboxSpec{}) {
		return sess.Sandbox
	}
	return a.Sandbox
}

func (a *Agent) toolDefs() []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0, len(a.Tools))
	for _, t := range a.Tools {
		defs = append(defs, t.Definition())
	}
	return defs
}
