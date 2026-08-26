// Package agentdb declares what an agent database is, independently of which one
// an application chose.
package agentdb

import (
	"context"
	"time"
)

// KV is a durable place for an application's own state, in the database its
// agent already uses. Keys and values are opaque to the store: what a key means
// and what a value decodes to are the application's business. A key is text; a
// value is bytes and may be anything.
type KV interface {
	// Get returns the value stored under key. A key that was never written is
	// not an error: found reports whether there was one.
	Get(ctx context.Context, key string) (value []byte, found bool, err error)

	// Put writes value under key, replacing whatever was there.
	Put(ctx context.Context, key string, value []byte) error

	// Delete removes key. Deleting a key that is not there is not an error.
	Delete(ctx context.Context, key string) error

	// List returns every entry whose key starts with prefix, ordered by key.
	// The empty prefix lists everything. Ordering is bytewise, so a key layout
	// that encodes a sortable field sorts by it.
	List(ctx context.Context, prefix string) ([]Entry, error)
}

// Entry is one stored value and what is known about it.
type Entry struct {
	Key     string
	Value   []byte
	Updated time.Time
}
