package agent

import (
	"context"
	"time"
)

// SandboxSpec names the sandbox a session runs in. It is persisted as part of a
// Session, so it must never carry secret values — only names an application can
// resolve.
type SandboxSpec struct {
	// Name is the sandbox's identity. A durable sandbox is adopted by name if it
	// already exists and created if it does not. For a non-durable sandbox the
	// name is purely advisory: backends may attach it as a label so humans can
	// read `docker ps`, but nothing is shared or adopted.
	Name string `json:"name,omitempty"`

	// Image is the backend-specific image reference, consumed as an opaque
	// string. Nothing here builds, tags, or forks images.
	Image string `json:"image"`

	// Durable opts a sandbox out of the ephemeral lifecycle: it outlives
	// handles, sessions, and runs, sleeps when idle, and is removed only by an
	// explicit destroy. It requires a Name.
	Durable bool `json:"durable,omitempty"`
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

	// Close releases the handle. It is detach, not destroy: a durable sandbox
	// outlives every handle to it, keeps its filesystem, and is removed only by
	// an explicit destroy — so nothing in the agent loop, including
	// `defer box.Close()`, may take one away. Backends are free to reclaim
	// resources for the non-durable sandboxes they created themselves, which is
	// what makes the ephemeral path "created on Acquire, gone on Close".
	Close() error
}

// SandboxFactory turns a spec into a live handle (remote in prod, fake in tests).
type SandboxFactory interface {
	Acquire(ctx context.Context, spec SandboxSpec) (Sandbox, error)
}

// Sleeper is an optional capability a Sandbox may implement. Sleep releases
// compute but preserves the filesystem; Wake makes Exec possible again.
// Implementations that cannot sleep simply do not implement it, and the registry
// then never tries to sleep them.
type Sleeper interface {
	Sleep(ctx context.Context) error
	Wake(ctx context.Context) error
}

// Destroyer is an optional SandboxFactory capability: remove a sandbox by name,
// whether or not anything currently holds a handle to it. It is the one path
// that takes a durable sandbox away, and it is what Registry.Destroy and
// Registry.Reconcile reach the backend through.
type Destroyer interface {
	Destroy(ctx context.Context, name string) error
}

// Lister is an optional SandboxFactory capability: report the sandboxes that
// actually exist on the backend. That is the ground truth Registry.Reconcile
// diffs its in-process belief against; there is deliberately no table of
// sandboxes anywhere that could drift from it.
type Lister interface {
	List(ctx context.Context) ([]SandboxInfo, error)
}

// SandboxState is whether a sandbox currently has compute.
type SandboxState string

const (
	SandboxAwake  SandboxState = "awake"
	SandboxAsleep SandboxState = "asleep"
)

// SandboxInfo is a sandbox as someone believes it to be: the registry's own
// belief from Snapshot, or the backend's ground truth from List. Callers decide
// what to log, graph, or show in a /status reply.
type SandboxInfo struct {
	Name    string
	State   SandboxState
	Durable bool

	// Image is the image a sandbox actually runs. A backend listing reports it
	// as ground truth, which is how a Reconcile-adopted sandbox — one nothing in
	// this process ever asked for — still says what it is. The registry's own
	// belief carries no image: it never looked.
	Image string

	Refs     int       // outstanding handles; always 0 in a backend listing
	LastExec time.Time // zero until a command has run
	DueAt    time.Time // next sleep deadline; zero when no timer is armed
}

// SandboxEventType names a sandbox transition worth reporting.
type SandboxEventType string

const (
	SandboxAcquired      SandboxEventType = "acquired"
	SandboxReleased      SandboxEventType = "released"
	SandboxSlept         SandboxEventType = "slept"
	SandboxWoke          SandboxEventType = "woke"
	SandboxSleepFailed   SandboxEventType = "sleep_failed"
	SandboxWakeFailed    SandboxEventType = "wake_failed"
	SandboxAdopted       SandboxEventType = "adopted"
	SandboxImageMismatch SandboxEventType = "image_mismatch"
)

// SandboxEvent is one reported transition. The core stays silent; observers wire
// slog or metrics themselves.
type SandboxEvent struct {
	Type SandboxEventType
	Name string

	Refs      int    // handles outstanding after an acquire or release
	Image     string // the image a sandbox actually runs (adopted, image_mismatch)
	WantImage string // the image the spec asked for (image_mismatch)
	Err       error  // why a sleep or wake failed
}
