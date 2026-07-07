package testutils

import (
	"context"

	"github.com/maikdotfi/metaharness/agent"
)

// MemStore is an in-memory SessionStore that just counts saves.
type MemStore struct{ Saves int }

func (s *MemStore) Save(context.Context, *agent.Session) error { s.Saves++; return nil }

func (s *MemStore) Load(context.Context, string) (*agent.Session, error) {
	return nil, agent.ErrNotFound
}
