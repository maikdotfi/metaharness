-- +goose Up
-- A session binds an agent + a machine and runs a single prompt to completion.
-- For this first slice the agent and machine are implicit (the only coding agent,
-- a local machine rooted at workdir); later these become real foreign keys.
CREATE TABLE sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    prompt     TEXT NOT NULL,
    workdir    TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'running', -- running | done | error
    final_text TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One row per transcript message, stored as the raw JSON the model layer
-- produced so we can round-trip it. seq orders them within a session.
CREATE TABLE session_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,           -- user | assistant | tool | system
    message    TEXT NOT NULL,           -- JSON-encoded transcript message
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_session_events_session ON session_events(session_id, seq);

-- +goose Down
DROP TABLE session_events;
DROP TABLE sessions;
