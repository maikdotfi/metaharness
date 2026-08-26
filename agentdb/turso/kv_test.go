package turso_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agentdb"
	"github.com/maikdotfi/metaharness/agentdb/turso"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestKV(t *testing.T) {
	testutils.RunKVSuite(t, func(t *testing.T) agentdb.KV {
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

// TestKVSurvivesTheProcess is the point of the table: an application's own state
// is still there when the process that wrote it is gone.
func TestKVSurvivesTheProcess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")

	store, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Put(ctx, "episode/12", []byte(`{"published":true}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, found, err := reopened.Get(ctx, "episode/12")
	if err != nil || !found {
		t.Fatalf("Get() = found %v, error %v, want the value the closed store wrote", found, err)
	}
	if string(got) != `{"published":true}` {
		t.Errorf("value = %q, want the value the closed store wrote", got)
	}
}

// TestKVCreatedAtSurvivesAWrite covers the upsert's omitted column: the row
// records when a key first appeared.
func TestKVCreatedAtSurvivesAWrite(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	if err := store.Put(ctx, "episode/12", []byte("first")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	var created string
	if err := db.QueryRowContext(ctx,
		"SELECT created_at FROM agent_kv WHERE key = ?", "episode/12").Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if err := store.Put(ctx, "episode/12", []byte("second")); err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx,
		"SELECT created_at FROM agent_kv WHERE key = ?", "episode/12").Scan(&got); err != nil {
		t.Fatalf("read created_at again: %v", err)
	}
	if got != created {
		t.Errorf("created_at = %q, want %q — the second write moved it", got, created)
	}
}

// TestKVAndNotesDoNotInterfere: two tables, two subsystems, one file.
func TestKVAndNotesDoNotInterfere(t *testing.T) {
	ctx := context.Background()
	store, _ := schemaStore(t)

	if err := store.Append(ctx, "taste", "Deep dives."); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := store.Put(ctx, "taste", []byte("a value, not a note")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	entries, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || string(entries[0].Value) != "a value, not a note" {
		t.Fatalf("List() = %#v, want only the value", entries)
	}
	notes, err := store.Notes(ctx)
	if err != nil {
		t.Fatalf("Notes() error = %v", err)
	}
	if len(notes) != 1 || notes[0].Content != "Deep dives." {
		t.Fatalf("Notes() = %#v, want only the note", notes)
	}
}
