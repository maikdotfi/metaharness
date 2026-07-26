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

func (h *handle) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	if h.closed.Load() {
		return agent.ExecResult{}, ErrClosed
	}
	return h.entry.exec(ctx, cmd)
}

// Close releases this handle. It is idempotent and has no lifecycle effect.
func (h *handle) Close() error {
	h.closed.Store(true)
	return nil
}
