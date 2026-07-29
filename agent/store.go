package agent

import (
	"context"
	"errors"
	"time"

	"charm.land/fantasy"
)

var ErrNotFound = errors.New("session not found")

type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
}

// SessionLister is an optional store capability for discovering resumable
// sessions.
type SessionLister interface {
	List(ctx context.Context, limit int) ([]SessionInfo, error)
}

// SessionInfo is the summary needed to choose a session without loading its
// transcript.
type SessionInfo struct {
	ID        string
	Model     string
	Status    Status
	Messages  int
	Usage     fantasy.Usage
	UpdatedAt time.Time
}

// SessionRecord is a session as a store sees it: the fields that survive a
// process, and nothing that cannot. A live sandbox handle is not one of them.
//
// It exists so a store can be written outside package agent. A session keeps its
// sandbox name unexported on purpose — the only thing that changes the name is
// Bind, which refuses a sandbox the session did not record — and a record is how
// that name reaches storage without handing callers a field to reassign.
type SessionRecord struct {
	ID       string
	Model    string
	Status   Status
	Usage    fantasy.Usage
	Sandbox  string // the sandbox's name; a handle is never persisted
	Messages []fantasy.Message
}

// Record returns what a store should write down for s.
func (s *Session) Record() SessionRecord {
	return SessionRecord{
		ID:       s.ID,
		Model:    s.Model,
		Status:   s.Status,
		Usage:    s.Usage,
		Sandbox:  s.name,
		Messages: s.Messages,
	}
}

// Session rebuilds the session r describes. It comes back unbound, which is what
// a resumed task looks like before Bind gives it a live sandbox again.
//
// Each call gets its own transcript: two sessions restored from one record would
// otherwise append into the same array and overwrite each other's turns.
func (r SessionRecord) Session() *Session {
	s := &Session{
		ID:     r.ID,
		Model:  r.Model,
		Status: r.Status,
		Usage:  r.Usage,
		name:   r.Sandbox,
	}
	if r.Messages != nil {
		s.Messages = make([]fantasy.Message, len(r.Messages))
		copy(s.Messages, r.Messages)
	}
	return s
}
