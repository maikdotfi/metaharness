package testutils

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/agentdb"
)

// MemKV is an agentdb.KV that keeps an application's state in memory and
// nothing else, so a test of one can skip the database.
type MemKV struct {
	mu      sync.Mutex
	entries []agentdb.Entry
	clock   int
}

var _ agentdb.KV = (*MemKV)(nil)

func (s *MemKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(key); i >= 0 {
		return bytes.Clone(s.entries[i].Value), true, nil
	}
	return nil, false, nil
}

func (s *MemKV) Put(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// A monotonic stand-in for a clock, so two writes in the same nanosecond
	// still order.
	s.clock++
	now := time.Unix(0, int64(s.clock))

	value = bytes.Clone(value)
	if i := s.index(key); i >= 0 {
		s.entries[i].Value, s.entries[i].Updated = value, now
		return nil
	}
	s.entries = append(s.entries, agentdb.Entry{Key: key, Value: value, Updated: now})
	return nil
}

func (s *MemKV) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(key); i >= 0 {
		s.entries = slices.Delete(s.entries, i, i+1)
	}
	return nil
}

func (s *MemKV) List(ctx context.Context, prefix string) ([]agentdb.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var found []agentdb.Entry
	for _, entry := range s.entries {
		if strings.HasPrefix(entry.Key, prefix) {
			entry.Value = bytes.Clone(entry.Value)
			found = append(found, entry)
		}
	}
	slices.SortFunc(found, func(a, b agentdb.Entry) int { return strings.Compare(a.Key, b.Key) })
	return found, nil
}

func (s *MemKV) index(key string) int {
	return slices.IndexFunc(s.entries, func(e agentdb.Entry) bool { return e.Key == key })
}
