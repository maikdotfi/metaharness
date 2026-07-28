package sandbox

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
)

// Spec is what a backend needs to make a sandbox that does not exist yet. The
// name is its identity and is required; everything else is creation
// configuration, used only when there is nothing under that name — once there
// is, the existing sandbox is what a caller gets, image and all.
//
// A Spec is never persisted. It describes how this process would create a
// sandbox, which is not something a session, or the next process, has any
// business remembering.
type Spec struct {
	Name  string
	Image string
}

// Backend is where sandboxes actually live, and is the ground truth for which
// ones exist. It carries out requested changes and reports what it finds; it
// decides nothing about lifecycle. Idle policy, serialization and timers all
// belong to the Manager above it.
//
// EnsureReady, Stop and Destroy are idempotent with respect to the lifecycle
// state they ask for. Exec is not.
type Backend interface {
	// EnsureReady makes the named sandbox exist and run: creating it from spec
	// if there is nothing under that name, starting it if it is stopped, and
	// leaving it alone if it is already running.
	EnsureReady(ctx context.Context, spec Spec) error

	// Exec runs one command in the named, running sandbox. A command that ran
	// and exited non-zero is an ExecResult, not an error; an error means the
	// command never ran or its result could not be established.
	Exec(ctx context.Context, name string, cmd agent.Command) (agent.ExecResult, error)

	// Stop releases the sandbox's compute and keeps its filesystem.
	Stop(ctx context.Context, name string) error

	// Destroy removes compute and filesystem. A sandbox that is already gone is
	// success.
	Destroy(ctx context.Context, name string) error

	// List reports what the backend has, without changing any of it.
	List(ctx context.Context) ([]BackendSandbox, error)

	// Close releases whatever the backend itself holds — a daemon connection,
	// say. It has no lifecycle effect: the sandboxes are left exactly as they
	// are, because outliving the process that used them is the point of them.
	//
	// A Manager owns the backend it was given and closes it during shutdown, so
	// this is not a method applications call. It is on the interface rather than
	// discovered through an io.Closer assertion so that ownership is visible in
	// the type instead of being a thing callers have to know.
	Close() error
}

// BackendState is whether a sandbox's compute is running, as the backend sees
// it. A stopped sandbox still exists and still has its filesystem.
type BackendState uint8

const (
	BackendStopped BackendState = iota
	BackendRunning
)

type BackendSandbox struct {
	Name  string
	Image string
	State BackendState
}
