package agent

import "context"

// Memory is what an agent knows between sessions. The kind of memory decides
// both how it reaches the model and what the model can do to it, so an
// implementation supplies its own tools rather than the application wiring them.
type Memory interface {
	// Recall returns text for the system prompt, or "" for nothing to add. It is
	// called once at the start of a turn, not once per model call.
	Recall(ctx context.Context) (string, error)

	// Tools are the memory's own tools. WithMemory registers them.
	Tools() []Tool
}

// WithMemory gives the agent notes that outlive its sessions, and registers the
// memory's own tools: the application hands the memory over once, not once for
// what it reads and again for what it writes.
func WithMemory(m Memory) Option {
	return func(a *Agent) {
		a.Memory = m
		if m != nil {
			WithTools(m.Tools()...)(a)
		}
	}
}

// systemPrompt is what the model reads for a whole turn: the application's
// prompt, and what the agent remembers appended to it. Resolved once, before the
// turn starts, so a note written during the turn lands on the next one.
func (a *Agent) systemPrompt(ctx context.Context) (string, error) {
	if a.Memory == nil {
		return a.SystemPrompt, nil
	}
	notes, err := a.Memory.Recall(ctx)
	if err != nil {
		return "", err
	}
	if notes == "" {
		return a.SystemPrompt, nil
	}
	return a.SystemPrompt + "\n\n" + notes, nil
}
