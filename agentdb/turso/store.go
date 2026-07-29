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

	"charm.land/fantasy"
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

// Store persists sessions as rows: one per session, and one per message. It can
// also list session summaries without reading a transcript.
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

// Save updates the session's row and appends the messages that have appeared
// since the last Save. It never rewrites a transcript, so the cost of a turn is
// the turn and not the history behind it.
//
// One transaction covers both, so a crash mid-turn leaves a session row and a
// transcript that agree. There is no separate Create: the session row is an
// upsert whose update list omits created_at, so the first Save records when the
// session started and later ones only move updated_at.
func (s *Store) Save(ctx context.Context, sess *agent.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess == nil {
		return errors.New("turso: save nil session")
	}
	rec := sess.Record()

	stored, err := s.storedCount(ctx, rec.ID)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("turso: begin save of session %q: %w", rec.ID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	now := s.now().UTC().Format(timestampFormat)
	if err := upsertSession(ctx, tx, rec, now); err != nil {
		return err
	}
	for seq := stored; seq < len(rec.Messages); seq++ { // usually one or two rows
		if err := insertMessage(ctx, tx, rec.ID, seq, rec.Messages[seq], now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("turso: commit save of session %q: %w", rec.ID, err)
	}
	return nil
}

// storedCount is how many of a session's messages the database already holds, and
// so where the next append starts.
//
// It is counted per Save on purpose: holding no state, it is right immediately
// after a Load, right after a process restart, and right for a Store serving many
// sessions at once — no map, no mutex, nothing to fall out of step. Caching it
// would save one indexed count on a local file, and the composite primary key
// means a stale cursor could only ever fail loudly anyway.
func (s *Store) storedCount(ctx context.Context, id string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_messages WHERE session_id = ?", id,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("turso: count stored messages of session %q: %w", id, err)
	}
	return count, nil
}

func upsertSession(ctx context.Context, tx *sql.Tx, rec agent.SessionRecord, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_sessions
			(id, model, status, sandbox,
			 input_tokens, output_tokens, total_tokens,
			 reasoning_tokens, cache_creation_tokens, cache_read_tokens,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			model = excluded.model,
			status = excluded.status,
			sandbox = excluded.sandbox,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			total_tokens = excluded.total_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			cache_creation_tokens = excluded.cache_creation_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			updated_at = excluded.updated_at`,
		rec.ID, rec.Model, string(rec.Status), rec.Sandbox,
		rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Usage.TotalTokens,
		rec.Usage.ReasoningTokens, rec.Usage.CacheCreationTokens, rec.Usage.CacheReadTokens,
		now, now,
	)
	if err != nil {
		return fmt.Errorf("turso: save session %q: %w", rec.ID, err)
	}
	return nil
}

// insertMessage appends one message. role is lifted out of the message so a
// database opened by hand is legible without piping every row through jq; it is
// written from the message and never read back, so it cannot diverge from the
// transcript Load returns.
//
// A plain INSERT, not INSERT OR IGNORE: a duplicate (session_id, seq) means a
// second writer, and losing that write loudly beats corrupting a transcript
// quietly.
func insertMessage(
	ctx context.Context, tx *sql.Tx, id string, seq int, msg fantasy.Message, now string,
) error {
	content, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("turso: encode message %d of session %q: %w", seq, id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_messages (session_id, seq, role, content_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, seq, string(msg.Role), string(content), now,
	); err != nil {
		return fmt.Errorf("turso: append message %d of session %q: %w", seq, id, err)
	}
	return nil
}

// Load returns the session, with the sandbox name it recorded and no live handle
// — which is what the resume path needs: Bind gives it a sandbox again.
//
// A session row with no messages is a real, empty session and loads as one:
// absence of messages is not absence of a session.
func (s *Store) Load(ctx context.Context, id string) (*agent.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rec := agent.SessionRecord{ID: id}
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT model, status, sandbox,
		       input_tokens, output_tokens, total_tokens,
		       reasoning_tokens, cache_creation_tokens, cache_read_tokens
		FROM agent_sessions
		WHERE id = ?`, id,
	).Scan(
		&rec.Model, &status, &rec.Sandbox,
		&rec.Usage.InputTokens, &rec.Usage.OutputTokens, &rec.Usage.TotalTokens,
		&rec.Usage.ReasoningTokens, &rec.Usage.CacheCreationTokens, &rec.Usage.CacheReadTokens,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, agent.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("turso: load session %q: %w", id, err)
	}
	rec.Status = agent.Status(status)

	if rec.Messages, err = s.loadMessages(ctx, id); err != nil {
		return nil, err
	}
	return rec.Session(), nil
}

// loadMessages reads a transcript back in the order it was appended. seq is the
// message's index in the session, so ORDER BY seq is the whole read path.
func (s *Store) loadMessages(ctx context.Context, id string) ([]fantasy.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, content_json FROM agent_messages WHERE session_id = ? ORDER BY seq", id)
	if err != nil {
		return nil, fmt.Errorf("turso: load messages of session %q: %w", id, err)
	}
	defer rows.Close()

	var messages []fantasy.Message
	for rows.Next() {
		var (
			seq     int
			content string
		)
		if err := rows.Scan(&seq, &content); err != nil {
			return nil, fmt.Errorf("turso: scan message of session %q: %w", id, err)
		}
		var msg fantasy.Message
		if err := json.Unmarshal([]byte(content), &msg); err != nil {
			return nil, fmt.Errorf("turso: decode message %d of session %q: %w", seq, id, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turso: load messages of session %q: %w", id, err)
	}
	return messages, nil
}

// List returns the most recently saved sessions first.
func (s *Store) List(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []agent.SessionInfo{}, nil
	}
	// The message count is a correlated subquery rather than a column: with a
	// message table the count has one source of truth, and one indexed count per
	// listed row is not worth keeping a second copy of it in step.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id,
			model,
			status,
			(SELECT COUNT(*) FROM agent_messages WHERE session_id = agent_sessions.id),
			input_tokens, output_tokens, total_tokens,
			reasoning_tokens, cache_creation_tokens, cache_read_tokens,
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
			updatedAt string
		)
		if err := rows.Scan(
			&info.ID, &info.Model, &status, &info.Messages,
			&info.Usage.InputTokens, &info.Usage.OutputTokens, &info.Usage.TotalTokens,
			&info.Usage.ReasoningTokens, &info.Usage.CacheCreationTokens,
			&info.Usage.CacheReadTokens,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("turso: scan session summary: %w", err)
		}
		info.Status = agent.Status(status)
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
