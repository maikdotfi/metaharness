package turso_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agentdb/turso"
	"github.com/maikdotfi/metaharness/memory"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestNoteStore(t *testing.T) {
	testutils.RunNoteStoreSuite(t, func(t *testing.T) memory.Store {
		t.Helper()
		store, err := turso.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})
		return store
	})
}

// TestNotesSurviveTheProcess is why notes are in a database at all: a session
// twice a day throws the transcript away, and what the agent knows has to still
// be there.
func TestNotesSurviveTheProcess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")

	store, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Append(ctx, "taste", "Deep dives."); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := reopened.Notes(ctx)
	if err != nil {
		t.Fatalf("Notes() error = %v", err)
	}
	if len(got) != 1 || got[0].Topic != "taste" || got[0].Content != "Deep dives." {
		t.Fatalf("Notes() = %#v, want the note the closed store wrote", got)
	}
}

// TestNoteCreatedAtSurvivesAWrite covers the upsert's omitted column: the first
// write records when a topic first came up, and later ones only move updated_at.
func TestNoteCreatedAtSurvivesAWrite(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	if err := store.Append(ctx, "taste", "Deep dives."); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	var created string
	if err := db.QueryRowContext(ctx,
		"SELECT created_at FROM agent_notes WHERE topic = ?", "taste").Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if err := store.Replace(ctx, "taste", "Interviews only."); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx,
		"SELECT created_at FROM agent_notes WHERE topic = ?", "taste").Scan(&got); err != nil {
		t.Fatalf("read created_at again: %v", err)
	}
	if got != created {
		t.Errorf("created_at = %q, want %q — the second write moved it", got, created)
	}
}
