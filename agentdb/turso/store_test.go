package turso_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

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
	saved := testutils.UserSession("durable", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	if err := store.Save(ctx, saved); err != nil {
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
		testutils.UserSession("timed", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello"),
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

// The tests below read the rows the store wrote, because for this store the
// schema is part of the contract: one of its promises is that a session database
// is inspectable by hand with the sqlite3 CLI. The behaviour every store shares
// is asserted by the suite above, which never looks at a row.

// storedTimestampFormat mirrors the text format the store writes into created_at
// and updated_at. It is pinned here because a database read by hand is only
// legible if its timestamps are.
const storedTimestampFormat = "2006-01-02T15:04:05.000000000Z"

// TestSaveWritesOneRowPerMessage pins the transcript as rows in order: seq dense
// from zero, and role readable without decoding the message.
func TestSaveWritesOneRowPerMessage(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	sess := testutils.UserSession("rows", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	sess.Messages = append(sess.Messages, testutils.AssistantText("hi"))
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	want := []string{"0:user", "1:assistant"}
	if got := messageRows(t, db, "rows"); !reflect.DeepEqual(got, want) {
		t.Errorf("message rows = %v, want %v", got, want)
	}
}

// TestSaveLeavesStoredMessagesAlone is append-only stated at the row level: a
// second Save adds the new message and does not rewrite the ones already there,
// which is what makes a turn's write cost the turn rather than the history.
func TestSaveLeavesStoredMessagesAlone(t *testing.T) {
	ctx := context.Background()
	var now time.Time
	store, db := schemaStore(t, turso.WithClock(func() time.Time { return now }))

	first := time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.July, 2, 10, 30, 0, 0, time.UTC)

	sess := testutils.UserSession("append", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	now = first
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	sess.Messages = append(sess.Messages, testutils.AssistantText("hi"))
	now = second
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	// The first message keeps the timestamp of the Save that wrote it; only the
	// second row is new.
	var created []string
	rows, err := db.QueryContext(ctx,
		"SELECT created_at FROM agent_messages WHERE session_id = ? ORDER BY seq", "append")
	if err != nil {
		t.Fatalf("query message timestamps: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan message timestamp: %v", err)
		}
		created = append(created, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read message timestamps: %v", err)
	}

	want := []string{
		first.UTC().Format(storedTimestampFormat),
		second.UTC().Format(storedTimestampFormat),
	}
	if !reflect.DeepEqual(created, want) {
		t.Errorf("message created_at = %v, want %v — the second Save rewrote a stored row", created, want)
	}
}

// TestDuplicateMessageSeqFails is the single-writer property the schema gives for
// free: because seq comes from the stored count and (session_id, seq) is unique, a
// second writer's first append is refused rather than interleaved into someone
// else's transcript.
func TestDuplicateMessageSeqFails(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	sess := testutils.UserSession("dup", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO agent_messages (session_id, seq, role, content_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		"dup", 0, "user", `{"role":"user"}`, "2026-01-01T00:00:00.000000000Z",
	)
	if err == nil {
		t.Fatal("a second message at seq 0: err = nil, want a constraint failure")
	}

	// And the transcript is still the one message the store wrote.
	want := []string{"0:user"}
	if got := messageRows(t, db, "dup"); !reflect.DeepEqual(got, want) {
		t.Errorf("message rows = %v, want %v", got, want)
	}
}

// TestFirstSaveRecordsCreatedAt covers the upsert's omitted column: the first
// Save records when the session started, and later ones only move updated_at.
func TestFirstSaveRecordsCreatedAt(t *testing.T) {
	ctx := context.Background()
	var now time.Time
	store, db := schemaStore(t, turso.WithClock(func() time.Time { return now }))

	created := time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC)
	updated := time.Date(2026, time.July, 2, 10, 30, 0, 0, time.UTC)

	sess := testutils.UserSession("timestamps", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	now = created
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}
	sess.Messages = append(sess.Messages, testutils.AssistantText("hi"))
	now = updated
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	var gotCreated, gotUpdated string
	if err := db.QueryRowContext(ctx,
		"SELECT created_at, updated_at FROM agent_sessions WHERE id = ?", "timestamps",
	).Scan(&gotCreated, &gotUpdated); err != nil {
		t.Fatalf("read session timestamps: %v", err)
	}
	if want := created.Format(storedTimestampFormat); gotCreated != want {
		t.Errorf("created_at = %q, want %q — the second Save moved it", gotCreated, want)
	}
	if want := updated.Format(storedTimestampFormat); gotUpdated != want {
		t.Errorf("updated_at = %q, want %q", gotUpdated, want)
	}
}

// TestUsageColumnsAreSummable is why usage is six integer columns and not a JSON
// document: "how many tokens did this agent burn" is a query, not a program.
func TestUsageColumnsAreSummable(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	for _, s := range []struct {
		id    string
		usage fantasy.Usage
	}{
		{"first", fantasy.Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}},
		{"second", fantasy.Usage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9}},
	} {
		sess := agent.SessionRecord{
			ID:      s.id,
			Model:   "model",
			Status:  agent.StatusCompleted,
			Sandbox: "work",
			Usage:   s.usage,
		}.Session()
		if err := store.Save(ctx, sess); err != nil {
			t.Fatalf("Save(%q) error = %v", s.id, err)
		}
	}

	var gotInput, gotTotal int64
	if err := db.QueryRowContext(ctx,
		"SELECT SUM(input_tokens), SUM(total_tokens) FROM agent_sessions",
	).Scan(&gotInput, &gotTotal); err != nil {
		t.Fatalf("sum usage columns: %v", err)
	}
	if gotInput != 17 || gotTotal != 22 {
		t.Errorf("SUM(input_tokens), SUM(total_tokens) = %d, %d; want 17, 22", gotInput, gotTotal)
	}
}

// TestLoadMalformedMessageReturnsDecodeError keeps a corrupt row loud and
// findable: never a panic, and never a silently truncated transcript.
func TestLoadMalformedMessageReturnsDecodeError(t *testing.T) {
	ctx := context.Background()
	store, db := schemaStore(t)

	sess := testutils.UserSession("broken", "model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_messages (session_id, seq, role, content_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		"broken", 1, "assistant", "{", "2026-01-01T00:00:00.000000000Z",
	); err != nil {
		t.Fatalf("insert malformed message: %v", err)
	}

	got, err := store.Load(ctx, "broken")
	if got != nil {
		t.Fatalf("Load() = %#v, want nil", got)
	}
	if err == nil || errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("Load() error = %v, want a decode error", err)
	}
	// It names the session and the seq, so the bad row can be found by hand.
	if !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "message 1") {
		t.Errorf("Load() error = %v, want it to name session %q and message 1", err, "broken")
	}
}

// schemaStore returns a migrated store together with the database under it, so a
// test can read the rows the store wrote.
func schemaStore(t *testing.T, opts ...turso.Option) (*turso.Store, *sql.DB) {
	t.Helper()
	db := openDB(t)
	if err := turso.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return turso.New(db, opts...), db
}

// messageRows reads a session's transcript as "seq:role" strings — the shape a
// person inspecting the database by hand would read.
func messageRows(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		"SELECT seq, role FROM agent_messages WHERE session_id = ? ORDER BY seq", id)
	if err != nil {
		t.Fatalf("query message rows: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var (
			seq  int
			role string
		)
		if err := rows.Scan(&seq, &role); err != nil {
			t.Fatalf("scan message row: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%s", seq, role))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read message rows: %v", err)
	}
	return got
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
