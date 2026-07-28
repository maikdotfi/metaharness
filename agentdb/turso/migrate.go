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

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE agent_sessions (
				id            TEXT PRIMARY KEY,
				model         TEXT NOT NULL,
				status        TEXT NOT NULL,
				message_count INTEGER NOT NULL DEFAULT 0,
				session_json  TEXT NOT NULL,
				created_at    TEXT NOT NULL,
				updated_at    TEXT NOT NULL
			)`,
			`CREATE INDEX agent_sessions_updated_at
				ON agent_sessions(updated_at DESC)`,
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
