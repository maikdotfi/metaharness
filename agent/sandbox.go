package agent

import "context"

// SandboxSpec names the sandbox an agent works in. The name is required and is
// the only thing that identifies it: the same name is the same filesystem
// tomorrow, in another session, after a restart. Image is creation
// configuration, used only when nothing with that name exists yet — once it
// does, the existing sandbox is what the agent gets.
type SandboxSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type Command struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox is a live handle. Never persisted.
//
// A handle is a reference, not the sandbox: opening and closing one says
// nothing about whether the sandbox exists or is running. Close releases the
// handle only.
type Sandbox interface {
	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	Close() error
}

// SandboxFactory binds a name to a live handle (a sandbox manager in prod, a
// fake in tests). Open does no I/O and starts nothing: the first Exec is what
// creates or wakes the sandbox.
type SandboxFactory interface {
	Open(spec SandboxSpec) (Sandbox, error)
}
