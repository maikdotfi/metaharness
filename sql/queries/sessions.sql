-- name: CreateSession :one
INSERT INTO sessions (prompt, workdir)
VALUES (?, ?)
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions
WHERE id = ? LIMIT 1;

-- name: ListSessions :many
SELECT * FROM sessions
ORDER BY created_at DESC;

-- name: FinishSession :one
UPDATE sessions
SET status = ?, final_text = ?, error = ?
WHERE id = ?
RETURNING *;

-- name: AppendSessionEvent :one
INSERT INTO session_events (session_id, seq, role, message)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListSessionEvents :many
SELECT * FROM session_events
WHERE session_id = ?
ORDER BY seq ASC;
