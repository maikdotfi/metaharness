package agent

import (
	"errors"

	"charm.land/fantasy"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// ErrSandboxMismatch is a session being bound to a sandbox other than the one it
// recorded.
var ErrSandboxMismatch = errors.New("agent: the sandbox is not the one the session recorded")

// Session is one bounded task: its transcript, and the sandbox that task runs
// in. One per goroutine; the thing the store persists.
//
// The sandbox is a binding, not configuration. A session gets it once, at
// construction, and keeps it for as long as it exists — every turn of the same
// task therefore works in the same filesystem. Only the name is persisted, so a
// session restored from the store has to be bound to a live handle again before
// it can run.
type Session struct {
	ID       string
	Model    string            // reassign to switch model within the provider
	Messages []fantasy.Message // the transcript IS fantasy's type
	Usage    fantasy.Usage
	Status   Status

	// name is the sandbox this session runs in, and box the live handle to it.
	// name outlives the handle: it is what a restored session is bound by, and
	// what Close leaves behind.
	name string
	box  Sandbox
}

// NewSession starts a task in box. The sandbox is a parameter because there is
// no such thing as a session without one, and it names itself, so nothing else
// has to be told which sandbox this is.
func NewSession(id, modelID string, box Sandbox) *Session {
	s := &Session{ID: id, Model: modelID, Status: StatusActive}
	if box != nil {
		s.name, s.box = box.Name(), box
	}
	return s
}

// Sandbox returns the live handle this session runs in, or nil if it has none —
// which is what a session freshly loaded from a store looks like.
func (s *Session) Sandbox() Sandbox { return s.box }

// SandboxName returns the name of the sandbox this session runs in. It survives
// persistence, so it is how a restored session finds its sandbox again.
func (s *Session) SandboxName() string { return s.name }

// Bind gives a restored session its sandbox back. The handle must be for the
// name the session recorded: resuming a task in a different filesystem is a
// mistake, not a choice. A session that already holds a live handle is refused
// rather than quietly leaving that handle to nobody.
func (s *Session) Bind(box Sandbox) error {
	if box == nil {
		return errors.New("agent: nil sandbox")
	}
	if s.box != nil {
		return errors.New("agent: the session already has a sandbox")
	}
	if s.name != "" && box.Name() != s.name {
		return ErrSandboxMismatch
	}
	s.name, s.box = box.Name(), box
	return nil
}

// Close releases the session's handle on its sandbox, and nothing else: the
// sandbox itself keeps running and keeps its filesystem, which is what makes it
// worth naming. The recorded name stays, so the session can be bound again.
func (s *Session) Close() error {
	box := s.box
	if box == nil {
		return nil
	}
	s.box = nil
	return box.Close()
}

func addUsage(dst *fantasy.Usage, u fantasy.Usage) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.TotalTokens += u.TotalTokens
	dst.ReasoningTokens += u.ReasoningTokens
	dst.CacheCreationTokens += u.CacheCreationTokens
	dst.CacheReadTokens += u.CacheReadTokens
}
