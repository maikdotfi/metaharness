package agent

import "context"

// DiscardStore accepts session checkpoints without retaining them. It does not
// clear or otherwise mutate the caller-owned Session, so its in-memory
// transcript remains available to the agent loop and subsequent turns.
type DiscardStore struct{}

func (DiscardStore) Save(context.Context, *Session) error { return nil }

func (DiscardStore) Load(context.Context, string) (*Session, error) {
	return nil, ErrNotFound
}
