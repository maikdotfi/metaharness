package testutils

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/maikdotfi/metaharness/agentdb"
)

// KVFactory creates an isolated key-value store for one test.
type KVFactory func(t *testing.T) agentdb.KV

// RunKVSuite checks the behavior shared by durable key-value stores. Keys and
// values are opaque here too: the suite stores bytes a codec would never
// produce, because the store is not allowed to care.
func RunKVSuite(t *testing.T, newStore KVFactory) {
	t.Helper()

	t.Run("missing key is not an error", func(t *testing.T) {
		value, found, err := newStore(t).Get(context.Background(), "show/never-written")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if found {
			t.Errorf("found = true, want false")
		}
		if value != nil {
			t.Errorf("value = %q, want nil", value)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		cases := map[string][]byte{
			"json":         []byte(`{"episode":12}`),
			"empty":        {},
			"invalid utf8": {0xff, 0xfe, 0x00, 0x80},
		}
		for name, want := range cases {
			t.Run(name, func(t *testing.T) {
				store := newStore(t)
				if err := store.Put(context.Background(), "show/one", want); err != nil {
					t.Fatalf("Put() error = %v", err)
				}
				got, found, err := store.Get(context.Background(), "show/one")
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				if !found {
					t.Fatal("found = false after a Put")
				}
				if !bytes.Equal(got, want) {
					t.Errorf("value = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("put replaces", func(t *testing.T) {
		store := newStore(t)
		if err := store.Put(context.Background(), "show/one", []byte("first")); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
		first := entry(t, store, "show/one")
		if first.Updated.IsZero() {
			t.Fatal("Updated is zero after a write")
		}

		if err := store.Put(context.Background(), "show/one", []byte("second")); err != nil {
			t.Fatalf("second Put() error = %v", err)
		}
		got, _, err := store.Get(context.Background(), "show/one")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if string(got) != "second" {
			t.Errorf("value = %q, want %q", got, "second")
		}
		if second := entry(t, store, "show/one").Updated; !second.After(first.Updated) {
			t.Errorf("Updated = %v after a second write, want it after %v", second, first.Updated)
		}
	})

	t.Run("delete removes the key", func(t *testing.T) {
		store := newStore(t)
		if err := store.Put(context.Background(), "show/one", []byte("first")); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
		if err := store.Delete(context.Background(), "show/one"); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if _, found, err := store.Get(context.Background(), "show/one"); err != nil || found {
			t.Fatalf("Get() = found %v, error %v, want found false and no error", found, err)
		}
	})

	t.Run("delete of an absent key", func(t *testing.T) {
		if err := newStore(t).Delete(context.Background(), "show/never-written"); err != nil {
			t.Errorf("Delete() error = %v", err)
		}
	})

	t.Run("list everything", func(t *testing.T) {
		store := seeded(t, newStore, "episode/2", "show/one", "episode/1")
		if got := keys(t, store, ""); !slices.Equal(got, []string{"episode/1", "episode/2", "show/one"}) {
			t.Errorf("List(\"\") = %q, want every key in order", got)
		}
	})

	t.Run("list a prefix", func(t *testing.T) {
		store := seeded(t, newStore, "show", "show/b", "show/a", "shy", "sho")
		want := []string{"show", "show/a", "show/b"}
		if got := keys(t, store, "show"); !slices.Equal(got, want) {
			t.Errorf("List(%q) = %q, want %q", "show", got, want)
		}
	})

	t.Run("list carries values", func(t *testing.T) {
		store := newStore(t)
		if err := store.Put(context.Background(), "show/one", []byte("first")); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
		got, err := store.List(context.Background(), "show/")
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(got) != 1 || !bytes.Equal(got[0].Value, []byte("first")) {
			t.Fatalf("List() = %#v, want one entry holding %q", got, "first")
		}
	})

	t.Run("empty range", func(t *testing.T) {
		store := seeded(t, newStore, "show/one")
		got, err := store.List(context.Background(), "episode/")
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("List() = %#v, want none", got)
		}
	})

	// LIKE would read _ as any character and % as any run of them, and both are
	// reasonable things to have in a key.
	t.Run("wildcards are literal", func(t *testing.T) {
		store := seeded(t, newStore, "show_a/one", "showXa/one", "100%/one", "100pc/one")
		if got := keys(t, store, "show_a"); !slices.Equal(got, []string{"show_a/one"}) {
			t.Errorf("List(%q) = %q, want only the underscore key", "show_a", got)
		}
		if got := keys(t, store, "100%"); !slices.Equal(got, []string{"100%/one"}) {
			t.Errorf("List(%q) = %q, want only the percent key", "100%", got)
		}
	})

	// A prefix ends where the next key after it begins, and every character is
	// entitled to have one — including the ones a byte cannot be incremented
	// into.
	t.Run("prefix bounds", func(t *testing.T) {
		cases := []struct {
			name   string
			prefix string
			keys   []string
			want   []string
		}{
			{
				name:   "multibyte",
				prefix: "caf\u00e9",
				keys:   []string{"caf\u00e9/one", "caf\u00e9\u00e9/two", "caf\u00ea/three"},
				want:   []string{"caf\u00e9/one", "caf\u00e9\u00e9/two"},
			},
			{
				// U+D7FF is the last character before the surrogates, which no
				// key can hold: the next key starts at U+E000.
				name:   "before the surrogates",
				prefix: "a\ud7ff",
				keys:   []string{"a\ud7ff/one", "a\ue000/two"},
				want:   []string{"a\ud7ff/one"},
			},
			{
				// Nothing sorts above the highest character, so the prefix has
				// no upper bound at all.
				name:   "highest character",
				prefix: "a\U0010ffff",
				keys:   []string{"a\U0010ffff/one", "b/two"},
				want:   []string{"a\U0010ffff/one"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store := seeded(t, newStore, tc.keys...)
				if got := keys(t, store, tc.prefix); !slices.Equal(got, tc.want) {
					t.Errorf("List(%q) = %q, want %q", tc.prefix, got, tc.want)
				}
			})
		}
	})

	// Bytewise, not a collation's idea of alphabetical: an application that
	// encodes a sortable field in a key gets back what it encoded.
	t.Run("bytewise order", func(t *testing.T) {
		store := seeded(t, newStore, "b", "A", "a", "B", "_")
		want := []string{"A", "B", "_", "a", "b"}
		if got := keys(t, store, ""); !slices.Equal(got, want) {
			t.Errorf("List(\"\") = %q, want %q", got, want)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.Put(ctx, "show/one", []byte("first")); !errors.Is(err, context.Canceled) {
			t.Errorf("Put() error = %v, want context.Canceled", err)
		}
		if _, _, err := store.Get(ctx, "show/one"); !errors.Is(err, context.Canceled) {
			t.Errorf("Get() error = %v, want context.Canceled", err)
		}
		if err := store.Delete(ctx, "show/one"); !errors.Is(err, context.Canceled) {
			t.Errorf("Delete() error = %v, want context.Canceled", err)
		}
		if _, err := store.List(ctx, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("List() error = %v, want context.Canceled", err)
		}
	})
}

func seeded(t *testing.T, newStore KVFactory, keys ...string) agentdb.KV {
	t.Helper()
	store := newStore(t)
	for _, key := range keys {
		if err := store.Put(context.Background(), key, []byte("value of "+key)); err != nil {
			t.Fatalf("Put(%q) error = %v", key, err)
		}
	}
	return store
}

func keys(t *testing.T, store agentdb.KV, prefix string) []string {
	t.Helper()
	entries, err := store.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q) error = %v", prefix, err)
	}
	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = e.Key
	}
	return got
}

func entry(t *testing.T, store agentdb.KV, key string) agentdb.Entry {
	t.Helper()
	for _, e := range keyEntries(t, store) {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry under %q", key)
	return agentdb.Entry{}
}

func keyEntries(t *testing.T, store agentdb.KV) []agentdb.Entry {
	t.Helper()
	entries, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List(\"\") error = %v", err)
	}
	return entries
}
