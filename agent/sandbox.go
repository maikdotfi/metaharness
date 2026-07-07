package agent

import "context"

type SandboxSpec struct {
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
type Sandbox interface {
	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	Close() error
}

// SandboxFactory turns a spec into a live handle (remote in prod, fake in tests).
type SandboxFactory interface {
	Acquire(ctx context.Context, spec SandboxSpec) (Sandbox, error)
}
