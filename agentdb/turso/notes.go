package turso

import (
	"context"
	"fmt"
	"time"

	"github.com/maikdotfi/metaharness/memory"
)

// Notes returns every note the agent holds, ordered by topic.
func (s *Store) Notes(ctx context.Context) ([]memory.Note, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT topic, content, updated_at FROM agent_notes ORDER BY topic")
	if err != nil {
		return nil, fmt.Errorf("turso: load notes: %w", err)
	}
	defer rows.Close()

	var notes []memory.Note
	for rows.Next() {
		var (
			note      memory.Note
			updatedAt string
		)
		if err := rows.Scan(&note.Topic, &note.Content, &updatedAt); err != nil {
			return nil, fmt.Errorf("turso: scan note: %w", err)
		}
		if note.Updated, err = time.Parse(timestampFormat, updatedAt); err != nil {
			return nil, fmt.Errorf("turso: decode updated time for note %q: %w", note.Topic, err)
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turso: load notes: %w", err)
	}
	return notes, nil
}

// Append adds a line to a topic's note, starting it if the topic is new.
func (s *Store) Append(ctx context.Context, topic, line string) error {
	return s.writeNote(ctx, topic, line,
		"agent_notes.content || char(10) || excluded.content")
}

// Replace makes content the whole of a topic's note.
func (s *Store) Replace(ctx context.Context, topic, content string) error {
	return s.writeNote(ctx, topic, content, "excluded.content")
}

// writeNote is one upsert: the content expression is the only thing appending and
// replacing disagree on, and doing it in SQL keeps a write one round trip rather
// than a read, a concatenation in Go, and a write. The update list omits
// created_at, so it records when a topic first came up.
func (s *Store) writeNote(ctx context.Context, topic, content, contentExpr string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC().Format(timestampFormat)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_notes (topic, content, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(topic) DO UPDATE SET
			content = `+contentExpr+`,
			updated_at = excluded.updated_at`,
		topic, content, now, now,
	)
	if err != nil {
		return fmt.Errorf("turso: write note %q: %w", topic, err)
	}
	return nil
}

var _ memory.Store = (*Store)(nil)
