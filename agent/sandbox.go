package agent

import "context"

type Command struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox is a live handle to the one sandbox a session works in. Never
// persisted.
//
// A handle is a reference, not the sandbox: opening and closing one says
// nothing about whether the sandbox exists or is running. Close releases the
// handle only.
//
// Name is the sandbox's whole identity, and it comes from the handle rather than
// from anything the agent was told: the same name is the same filesystem
// tomorrow, in another session, after a restart. Where sandboxes live and what
// they are made from is the business of whoever handed the handle over.
type Sandbox interface {
	Name() string
	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	Close() error
}
