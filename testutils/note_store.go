package testutils

import (
	"context"
	"errors"
	"testing"

	"github.com/maikdotfi/metaharness/memory"
)

// NoteStoreFactory creates an isolated note store for one test.
type NoteStoreFactory func(t *testing.T) memory.Store

// RunNoteStoreSuite checks the behavior shared by memory stores. It never looks
// at how a store keeps a note, and it never asserts the order Notes returns:
// what reaches the model is ordered by whoever renders it.
func RunNoteStoreSuite(t *testing.T, newStore NoteStoreFactory) {
	t.Helper()

	t.Run("append creates a topic", func(t *testing.T) {
		store := newStore(t)
		if err := store.Append(context.Background(), "taste", "Deep dives."); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if got := note(t, store, "taste"); got != "Deep dives." {
			t.Errorf("note = %q, want %q", got, "Deep dives.")
		}
	})

	// The model is not asked to restate what it already wrote down, so a second
	// line about one topic joins the first, in the order they were said.
	t.Run("append keeps both lines", func(t *testing.T) {
		store := newStore(t)
		for _, line := range []string{"Deep dives.", "Nothing over three hours."} {
			if err := store.Append(context.Background(), "taste", line); err != nil {
				t.Fatalf("Append(%q) error = %v", line, err)
			}
		}
		want := "Deep dives.\nNothing over three hours."
		if got := note(t, store, "taste"); got != want {
			t.Errorf("note = %q, want %q", got, want)
		}
	})

	t.Run("replace overwrites", func(t *testing.T) {
		store := newStore(t)
		if err := store.Append(context.Background(), "taste", "Deep dives."); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if err := store.Replace(context.Background(), "taste", "Interviews only."); err != nil {
			t.Fatalf("Replace() error = %v", err)
		}
		if got := note(t, store, "taste"); got != "Interviews only." {
			t.Errorf("note = %q, want %q", got, "Interviews only.")
		}
	})

	t.Run("replace creates a topic", func(t *testing.T) {
		store := newStore(t)
		if err := store.Replace(context.Background(), "taste", "Interviews only."); err != nil {
			t.Fatalf("Replace() error = %v", err)
		}
		if got := note(t, store, "taste"); got != "Interviews only." {
			t.Errorf("note = %q, want %q", got, "Interviews only.")
		}
	})

	t.Run("two topics do not mix", func(t *testing.T) {
		store := newStore(t)
		if err := store.Append(context.Background(), "taste", "Deep dives."); err != nil {
			t.Fatalf("Append(taste) error = %v", err)
		}
		if err := store.Append(context.Background(), "audience", "One listener."); err != nil {
			t.Fatalf("Append(audience) error = %v", err)
		}
		if got := note(t, store, "taste"); got != "Deep dives." {
			t.Errorf("taste = %q, want %q", got, "Deep dives.")
		}
		if got := note(t, store, "audience"); got != "One listener." {
			t.Errorf("audience = %q, want %q", got, "One listener.")
		}
	})

	// An agent that has been told nothing yet has no notes, which is a state and
	// not an error.
	t.Run("no notes", func(t *testing.T) {
		got, err := newStore(t).Notes(context.Background())
		if err != nil {
			t.Fatalf("Notes() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Notes() = %#v, want none", got)
		}
	})

	t.Run("timestamps move", func(t *testing.T) {
		store := newStore(t)
		if err := store.Append(context.Background(), "taste", "Deep dives."); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		first := notes(t, store)[0].Updated
		if first.IsZero() {
			t.Fatal("Updated is zero after a write")
		}
		if err := store.Append(context.Background(), "taste", "Nothing over three hours."); err != nil {
			t.Fatalf("second Append() error = %v", err)
		}
		if second := notes(t, store)[0].Updated; !second.After(first) {
			t.Errorf("Updated = %v after a second write, want it after %v", second, first)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.Append(ctx, "taste", "Deep dives."); !errors.Is(err, context.Canceled) {
			t.Errorf("Append() error = %v, want context.Canceled", err)
		}
		if err := store.Replace(ctx, "taste", "Deep dives."); !errors.Is(err, context.Canceled) {
			t.Errorf("Replace() error = %v, want context.Canceled", err)
		}
		if _, err := store.Notes(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Notes() error = %v, want context.Canceled", err)
		}
	})
}

func notes(t *testing.T, store memory.Store) []memory.Note {
	t.Helper()
	got, err := store.Notes(context.Background())
	if err != nil {
		t.Fatalf("Notes() error = %v", err)
	}
	return got
}

// note returns one topic's content, failing the test if the store does not hold
// it. Notes are looked up rather than indexed, because the order they come back
// in is the store's business.
func note(t *testing.T, store memory.Store, topic string) string {
	t.Helper()
	for _, n := range notes(t, store) {
		if n.Topic == topic {
			return n.Content
		}
	}
	t.Fatalf("no note about %q", topic)
	return ""
}
