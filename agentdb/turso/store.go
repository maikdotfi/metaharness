// Package turso stores agent sessions in a local embedded Turso database.
package turso

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tursodrv "turso.tech/database/tursogo"

	"github.com/maikdotfi/metaharness/agent"
)

const timestampFormat = "2006-01-02T15:04:05.000000000Z"

type config struct {
	now         func() time.Time
	busyTimeout *int
}

func defaultConfig() config {
	return config{now: time.Now}
}

// Option configures a Store.
type Option func(*config)

// WithClock supplies the clock used for session timestamps.
func WithClock(now func() time.Time) Option {
	return func(cfg *config) {
		if now != nil {
			cfg.now = now
		}
	}
}

// WithBusyTimeout sets the driver's lock wait in milliseconds. It is used by
// Open; databases passed to New retain their existing driver configuration.
func WithBusyTimeout(ms int) Option {
	return func(cfg *config) {
		cfg.busyTimeout = &ms
	}
}

// Store persists complete session snapshots and can list their summaries.
type Store struct {
	db     *sql.DB
	now    func() time.Time
	ownsDB bool
}

// Open opens a local Turso database, applies its schema, and returns a Store
// that owns the database.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	cfg := applyOptions(opts)
	connectorOptions := make([]tursodrv.ConnectorOption, 0, 1)
	if cfg.busyTimeout != nil {
		connectorOptions = append(connectorOptions, tursodrv.WithBusyTimeout(*cfg.busyTimeout))
	}
	connector, err := tursodrv.NewConnector(path, connectorOptions...)
	if err != nil {
		return nil, fmt.Errorf("turso: open connector: %w", err)
	}
	db := sql.OpenDB(connector)
	if strings.HasPrefix(path, ":memory:") {
		// Each in-memory driver connection is a different database.
		db.SetMaxOpenConns(1)
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: cfg.now, ownsDB: true}, nil
}

// New wraps a caller-provided database. The caller retains responsibility for
// applying Migrate and closing db.
func New(db *sql.DB, opts ...Option) *Store {
	cfg := applyOptions(opts)
	return &Store{db: db, now: cfg.now}
}

func applyOptions(opts []Option) config {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// Close closes databases created by Open. It does not close a database passed
// to New.
func (s *Store) Close() error {
	if s == nil || !s.ownsDB || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Save inserts or replaces the mutable fields of a complete session snapshot.
func (s *Store) Save(ctx context.Context, sess *agent.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess == nil {
		return errors.New("turso: save nil session")
	}
	payload, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("turso: encode session %q: %w", sess.ID, err)
	}
	now := s.now().UTC().Format(timestampFormat)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_sessions
			(id, model, status, message_count, session_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			model = excluded.model,
			status = excluded.status,
			message_count = excluded.message_count,
			session_json = excluded.session_json,
			updated_at = excluded.updated_at`,
		sess.ID, sess.Model, string(sess.Status), len(sess.Messages), string(payload), now, now,
	)
	if err != nil {
		return fmt.Errorf("turso: save session %q: %w", sess.ID, err)
	}
	return nil
}

// Load returns a complete session snapshot.
func (s *Store) Load(ctx context.Context, id string) (*agent.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx,
		"SELECT session_json FROM agent_sessions WHERE id = ?", id,
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, agent.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("turso: load session %q: %w", id, err)
	}
	var sess agent.Session
	if err := json.Unmarshal([]byte(payload), &sess); err != nil {
		return nil, fmt.Errorf("turso: decode session %q: %w", id, err)
	}
	return &sess, nil
}

// List returns the most recently saved sessions first.
func (s *Store) List(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []agent.SessionInfo{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id,
			model,
			status,
			message_count,
			json_extract(session_json, '$.Usage'),
			updated_at
		FROM agent_sessions
		ORDER BY updated_at DESC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("turso: list sessions: %w", err)
	}
	defer rows.Close()

	infos := make([]agent.SessionInfo, 0, limit)
	for rows.Next() {
		var (
			info      agent.SessionInfo
			status    string
			usageJSON string
			updatedAt string
		)
		if err := rows.Scan(
			&info.ID, &info.Model, &status, &info.Messages, &usageJSON, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("turso: scan session summary: %w", err)
		}
		info.Status = agent.Status(status)
		if err := json.Unmarshal([]byte(usageJSON), &info.Usage); err != nil {
			return nil, fmt.Errorf("turso: decode usage for session %q: %w", info.ID, err)
		}
		info.UpdatedAt, err = time.Parse(timestampFormat, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("turso: decode updated time for session %q: %w", info.ID, err)
		}
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turso: list sessions: %w", err)
	}
	return infos, nil
}

var _ agent.SessionStore = (*Store)(nil)
var _ agent.SessionLister = (*Store)(nil)
