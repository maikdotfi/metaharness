package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/maikdotfi/metaharness/agentdb"
)

// Get returns the value stored under key, and whether there was one.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var value []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM agent_kv WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("turso: get %q: %w", key, err)
	}
	return value, true, nil
}

// Put writes value under key. The update list omits created_at, so the row
// records when the key first appeared.
func (s *Store) Put(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value == nil {
		value = []byte{} // the column is NOT NULL, and no value is an empty one
	}
	now := s.now().UTC().Format(timestampFormat)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_kv (key, value, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`,
		key, value, now, now,
	)
	if err != nil {
		return fmt.Errorf("turso: put %q: %w", key, err)
	}
	return nil
}

// Delete removes key, whether or not it was there.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM agent_kv WHERE key = ?", key); err != nil {
		return fmt.Errorf("turso: delete %q: %w", key, err)
	}
	return nil
}

// List returns every entry whose key starts with prefix, ordered by key.
//
// The bound is a range rather than LIKE: `_` and `%` are wildcards there, and
// both are ordinary characters in a key. The range also walks the primary key
// index without depending on how the planner feels about LIKE.
func (s *Store) List(ctx context.Context, prefix string) ([]agentdb.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := "SELECT key, value, updated_at FROM agent_kv"
	args := []any{}
	if prefix != "" {
		query += " WHERE key >= ?"
		args = append(args, prefix)
		if next, ok := successor(prefix); ok {
			query += " AND key < ?"
			args = append(args, next)
		}
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY key", args...)
	if err != nil {
		return nil, fmt.Errorf("turso: list %q: %w", prefix, err)
	}
	defer rows.Close()

	var entries []agentdb.Entry
	for rows.Next() {
		var (
			entry     agentdb.Entry
			updatedAt string
		)
		if err := rows.Scan(&entry.Key, &entry.Value, &updatedAt); err != nil {
			return nil, fmt.Errorf("turso: scan entry: %w", err)
		}
		if entry.Updated, err = time.Parse(timestampFormat, updatedAt); err != nil {
			return nil, fmt.Errorf("turso: decode updated time for %q: %w", entry.Key, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turso: list %q: %w", prefix, err)
	}
	return entries, nil
}

// successor is the first key that no longer starts with prefix: the prefix with
// its last rune incremented, dropping trailing runes that cannot be. A prefix
// that is empty, or entirely the highest rune, has no upper bound.
//
// By rune and not by byte, because the column is TEXT and the driver rejects a
// parameter that is not valid UTF-8 — "caf\u00e9" incremented bytewise is not a
// string. UTF-8 sorts in code point order, so the two agree on ordering.
func successor(prefix string) (string, bool) {
	for prefix != "" {
		r, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
		if next, ok := nextRune(r); ok {
			return prefix + string(next), true
		}
	}
	return "", false
}

// nextRune is the smallest rune above r, skipping the surrogate range, which
// UTF-8 cannot encode.
func nextRune(r rune) (rune, bool) {
	r++
	if r == 0xd800 {
		r = 0xe000
	}
	if r > utf8.MaxRune {
		return 0, false
	}
	return r, true
}

var _ agentdb.KV = (*Store)(nil)
