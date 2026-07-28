package sandbox

import (
	"context"
	"sync/atomic"

	"github.com/maikdotfi/metaharness/agent"
)

// handle is one caller's reference to a named sandbox. It holds no state of its
// own beyond whether it is still usable: closing it releases the reference and
// nothing else, so an agent run ending never takes the sandbox with it.
type handle struct {
	entry  *entry
	closed atomic.Bool
}

var _ agent.Sandbox = (*handle)(nil)

// Name is the sandbox this handle refers to, and the whole of its identity: a
// caller that has a handle needs nothing else to say which sandbox it is in.
func (h *handle) Name() string { return h.entry.spec.Name }

func (h *handle) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	if h.closed.Load() {
		return agent.ExecResult{}, ErrClosed
	}
	// The first command is where the manager stops guessing about what it
	// inherited, and it happens here rather than inside the sandbox's goroutine:
	// the pass asks other sandboxes what state to record, which a goroutine
	// already serving this command could not answer for.
	h.entry.mgr.ensureAdopted(ctx)
	return h.entry.ask(request{kind: reqExec, ctx: ctx, cmd: cmd})
}

// Close releases this handle. It is idempotent and has no lifecycle effect.
func (h *handle) Close() error {
	h.closed.Store(true)
	return nil
}
