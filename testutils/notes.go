package testutils

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/memory"
)

// MemNotes is a memory.Store that keeps notes in memory and nothing else.
//
// It hands them back newest first, which is deliberately not the order they
// reach the model in: a memory that depended on the store's order would pass a
// suite and still churn the prompt cache.
type MemNotes struct {
	mu    sync.Mutex
	notes []memory.Note
	clock int
}

var _ memory.Store = (*MemNotes)(nil)

func (s *MemNotes) Notes(ctx context.Context) ([]memory.Note, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := slices.Clone(s.notes)
	slices.SortFunc(notes, func(a, b memory.Note) int { return b.Updated.Compare(a.Updated) })
	return notes, nil
}

func (s *MemNotes) Append(ctx context.Context, topic, line string) error {
	return s.write(ctx, topic, line, false)
}

func (s *MemNotes) Replace(ctx context.Context, topic, content string) error {
	return s.write(ctx, topic, content, true)
}

func (s *MemNotes) write(ctx context.Context, topic, content string, replace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// A monotonic stand-in for a clock, so two writes in the same nanosecond
	// still order.
	s.clock++
	now := time.Unix(0, int64(s.clock))

	if i := slices.IndexFunc(s.notes, func(n memory.Note) bool { return n.Topic == topic }); i >= 0 {
		if !replace {
			content = s.notes[i].Content + "\n" + content
		}
		s.notes[i].Content, s.notes[i].Updated = content, now
		return nil
	}
	s.notes = append(s.notes, memory.Note{Topic: topic, Content: content, Updated: now})
	return nil
}
