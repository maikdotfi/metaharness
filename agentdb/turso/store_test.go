package turso_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/agentdb/turso"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestStore(t *testing.T) {
	testutils.RunSessionStoreSuite(t, func(t *testing.T) agent.SessionStore {
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

func TestOpenPersistsSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	store, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Save(ctx, testutils.UserSession("durable", "model", "hello")); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := turso.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Load(ctx, "durable"); err != nil {
		t.Fatalf("Load() after reopen error = %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openDB(t)
	if err := turso.Migrate(context.Background(), db); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if err := turso.Migrate(context.Background(), db); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM agent_schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration rows = %d, want 1", count)
	}
}

func TestNewDoesNotOwnDatabase(t *testing.T) {
	db := openDB(t)
	if err := turso.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store := turso.New(db)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("provided database was closed: %v", err)
	}
}

func TestWithClockControlsUpdatedAt(t *testing.T) {
	want := time.Date(2026, time.July, 28, 9, 30, 0, 123, time.UTC)
	store, err := turso.Open(
		context.Background(),
		":memory:",
		turso.WithClock(func() time.Time { return want }),
	)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Save(
		context.Background(),
		testutils.UserSession("timed", "model", "hello"),
	); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	infos, err := store.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 1 || !infos[0].UpdatedAt.Equal(want) {
		t.Fatalf("List() = %#v, want updated time %v", infos, want)
	}
}

func TestLoadMalformedSessionReturnsDecodeError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if err := turso.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_sessions
			(id, model, status, message_count, session_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"broken", "model", "active", 0, "{", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert malformed session: %v", err)
	}

	got, err := turso.New(db).Load(ctx, "broken")
	if got != nil {
		t.Fatalf("Load() = %#v, want nil", got)
	}
	if err == nil || errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("Load() error = %v, want decode error", err)
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("turso", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
