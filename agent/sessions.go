package agent

import (
	"context"
	"fmt"
)

// Sessions is the stored history of an agent, and the two things an application
// does with it: show what there is, and put one of them back to work.
//
// Resume is Load, Open and Bind in that order, which is the whole of what a
// resumed task needs and easy to get wrong once: a session loaded but not bound
// has nowhere to run.
type Sessions interface {
	List(ctx context.Context, limit int) ([]SessionInfo, error)
	Resume(ctx context.Context, id string) (*Session, error)
}

// Sessions returns the agent's stored history, resumable into sandboxes from
// boxes, or nil if there is no history to offer: the store retains nothing — the
// default — or there is nowhere to open the sandboxes it recorded. Nil is the
// answer to "is there any history?", so a caller can leave the commands over it
// out entirely rather than offering ones that cannot work.
func (a *Agent) Sessions(boxes SandboxOpener) Sessions {
	lister, ok := a.Store.(SessionLister)
	if !ok || boxes == nil {
		return nil
	}
	return storedSessions{store: a.Store, lister: lister, boxes: boxes}
}

// storedSessions is a store paired with somewhere to open sandboxes. Neither
// half can resume a session alone: the store keeps a sandbox's name and never a
// handle, and only the manager can turn that name back into one.
type storedSessions struct {
	store  SessionStore
	lister SessionLister
	boxes  SandboxOpener
}

func (s storedSessions) List(ctx context.Context, limit int) ([]SessionInfo, error) {
	return s.lister.List(ctx, limit)
}

// Resume loads the session, opens the sandbox it recorded and binds the two, so
// the task continues in the filesystem it started in. Nothing comes back unless
// all three worked: a caller that already has a session running keeps it.
func (s storedSessions) Resume(ctx context.Context, id string) (*Session, error) {
	sess, err := s.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	box, err := s.boxes.Open(sess.SandboxName())
	if err != nil {
		return nil, fmt.Errorf("agent: opening sandbox %q for session %q: %w", sess.SandboxName(), id, err)
	}
	if err := sess.Bind(box); err != nil {
		// The handle has nowhere to go, and a handle nobody holds is a leak; the
		// sandbox itself is untouched by releasing it.
		_ = box.Close()
		return nil, err
	}
	return sess, nil
}
