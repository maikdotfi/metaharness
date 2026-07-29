package testutils

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

// MemStore is a SessionStore that keeps sessions in memory and nothing else. It
// retains what it is given, so a session saved to it can be loaded back and
// resumed — which is all a test needs to exercise the resume path without a file
// or a database.
//
// One MemStore serves many sessions and is safe to share across goroutines,
// because an agent serving concurrent sessions saves them all through one store.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]memSession
}

// memSession is what MemStore retains: the record a store sees, plus when it
// last saw it, which is the only thing List needs that a record does not carry.
type memSession struct {
	record    agent.SessionRecord
	updatedAt time.Time
}

func (s *MemStore) Save(ctx context.Context, sess *agent.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]memSession)
	}
	s.sessions[sess.ID] = memSession{record: sess.Record(), updatedAt: time.Now()}
	return nil
}

func (s *MemStore) Load(ctx context.Context, id string) (*agent.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.sessions[id]
	if !ok {
		return nil, agent.ErrNotFound
	}
	return stored.record.Session(), nil
}

// List returns the most recently saved sessions first.
func (s *MemStore) List(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []agent.SessionInfo{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	infos := make([]agent.SessionInfo, 0, len(s.sessions))
	for _, stored := range s.sessions {
		infos = append(infos, agent.SessionInfo{
			ID:        stored.record.ID,
			Model:     stored.record.Model,
			Status:    stored.record.Status,
			Messages:  len(stored.record.Messages),
			Usage:     stored.record.Usage,
			UpdatedAt: stored.updatedAt,
		})
	}
	slices.SortFunc(infos, func(a, b agent.SessionInfo) int {
		if a.UpdatedAt.Equal(b.UpdatedAt) {
			return strings.Compare(a.ID, b.ID)
		}
		return b.UpdatedAt.Compare(a.UpdatedAt) // most recent first
	})
	if len(infos) > limit {
		infos = infos[:limit]
	}
	return infos, nil
}

var (
	_ agent.SessionStore  = (*MemStore)(nil)
	_ agent.SessionLister = (*MemStore)(nil)
)
