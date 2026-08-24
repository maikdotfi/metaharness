package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type migration struct {
	version    int
	statements []string
}

// A session is two tables: one row in agent_sessions, and one row per message in
// agent_messages. Every Save appends the messages that appeared since the last
// one, so a turn's write cost is the turn rather than the whole transcript.
//
// sandbox holds the sandbox's name and nothing else. An image, a backend, a
// daemon address are the writing process's configuration, and the process that
// resumes the session is free to differ on all of them — the name is the only
// part that still means the same thing.
//
// Usage is six columns rather than a JSON document so that List scans integers,
// and so "how many tokens did this agent burn this month" is a SUM rather than a
// program.
//
// There is deliberately no foreign key: Turso's PRAGMA support is partial and
// SQLite needs foreign_keys=ON to enforce one anyway, so a REFERENCES clause here
// would read like a guarantee it is not. A delete path, if one ever exists,
// deletes messages explicitly in the same transaction.
var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE agent_sessions (
				id                    TEXT PRIMARY KEY,
				model                 TEXT NOT NULL,
				status                TEXT NOT NULL,
				sandbox               TEXT NOT NULL DEFAULT '',
				input_tokens          INTEGER NOT NULL DEFAULT 0,
				output_tokens         INTEGER NOT NULL DEFAULT 0,
				total_tokens          INTEGER NOT NULL DEFAULT 0,
				reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
				cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
				cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
				created_at            TEXT NOT NULL,
				updated_at            TEXT NOT NULL
			)`,
			`CREATE INDEX agent_sessions_updated_at
				ON agent_sessions(updated_at DESC)`,
			// seq is the message's index in the session's transcript, so the
			// transcript's order is the database's order. The composite primary key
			// is what makes a second writer fail loudly instead of interleaving.
			`CREATE TABLE agent_messages (
				session_id   TEXT NOT NULL,
				seq          INTEGER NOT NULL,
				role         TEXT NOT NULL,
				content_json TEXT NOT NULL,
				created_at   TEXT NOT NULL,
				PRIMARY KEY (session_id, seq)
			)`,
		},
	},
	{
		// What the agent knows between sessions: one row per topic, and no agent
		// id. This is a personal agent and the database is its; a second agent
		// gets a second file, the way a second sandbox gets a second name.
		version: 2,
		statements: []string{
			`CREATE TABLE agent_notes (
				topic      TEXT PRIMARY KEY,
				content    TEXT NOT NULL,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
	},
}

// Migrate applies all pending schema migrations in version order.
func Migrate(ctx context.Context, db *sql.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if db == nil {
		return errors.New("turso: migrate nil database")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("turso: create migrations table: %w", err)
	}

	for _, migration := range migrations {
		var exists int
		err := db.QueryRowContext(ctx,
			"SELECT 1 FROM agent_schema_migrations WHERE version = ?", migration.version,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("turso: inspect migration %d: %w", migration.version, err)
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("turso: begin migration %d: %w", migration.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range migration.statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("turso: apply migration %d: %w", migration.version, err)
		}
	}
	appliedAt := time.Now().UTC().Format(timestampFormat)
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO agent_schema_migrations (version, applied_at) VALUES (?, ?)",
		migration.version, appliedAt,
	); err != nil {
		return fmt.Errorf("turso: record migration %d: %w", migration.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("turso: commit migration %d: %w", migration.version, err)
	}
	return nil
}
