package testutils

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
)

// MemStore is an in-memory SessionStore. It counts saves and keeps a copy of
// each session as it was handed over, so a test can assert what the store
// received at that point in the run rather than what the session looks like once
// the run is over.
type MemStore struct {
	Saves     int
	Snapshots []agent.Session
}

func (s *MemStore) Save(_ context.Context, sess *agent.Session) error {
	s.Saves++
	s.Snapshots = append(s.Snapshots, *sess)
	return nil
}

func (s *MemStore) Load(context.Context, string) (*agent.Session, error) {
	return nil, agent.ErrNotFound
}
