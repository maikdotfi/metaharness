# Agent Database Plan

## Direction

Meta Harness treats persistence as an agent database: one storage backend per
agent, chosen by the application assembling the agent. The agent database owns
sessions, messages, and later any other agent-local state.

The embedded backend is **Turso** (`turso.tech/database/tursogo`), already in
`go.mod`. It speaks the SQLite query language and file format, registers a
`database/sql` driver, runs in-process, and needs no CGO — it reaches its Rust
core through `purego`.

**A session is two tables, not one blob.** A row in `agent_sessions` and one row
per message in `agent_messages`. Every `Save` appends the messages that appeared
since the last one; it never rewrites the transcript. The previous version of
this plan stored the whole session as `session_json` and deferred normalizing
"until a caller needs queryable message history" — that deferral was wrong. The
cost of a blob is paid on every loop iteration, by every session, forever, and it
grows with the transcript. A message table is not the expensive option here; it
is the cheap one.

The core `agent` package stays storage-agnostic. It defines the narrow interface
at the point of use; only the backend package imports a driver.

## Goals

- Append one row per message. A turn's write cost is the turn, not the history.
- Persist the sandbox a session ran in.
- Keep the library usable without a database: `DiscardStore` stays the default,
  and an application that wants no persistence links no driver.
- One durable store, not a choice of formats — see "One store, not two".
- Keep the SQL simple enough to read in one sitting and inspect by hand with the
  `sqlite3` CLI.
- Keep backend APIs small until a real caller needs more surface area.
- No code generation. Plain `database/sql` is enough.

## Non-Goals

- No server-specific schema, no multi-agent database model.
- No second durable backend "for portability". If Turso disappoints, the answer
  is a mainline-SQLite driver behind this schema, not a second format.
- No sqlc or goose until the schema is large enough to justify them.
- No part-level schema. A message row holds one JSON-encoded `fantasy.Message`;
  see "Normalize the transcript, not the message".
- No remote sync. Turso offers it; the store is a local embedded database.
- No memory schema yet.
- No write-lease enforcement yet — see "Single writer".

## What is in the tree today

`4cf2ea2` landed `agentdb/turso` with the blob schema, and `d07dca5` had already
replaced `agent.SandboxSpec` with the sandbox *name* the session records. Those
two commits disagree, so three things are true right now:

1. **The tree does not compile.** `testutils/session_store.go:164` still builds a
   `agent.SandboxSpec{Image: …}`. `go build ./...` fails.
2. **The Turso store silently loses the sandbox.** `Save` does
   `json.Marshal(sess)`, and the sandbox name lives in `Session.name`, which is
   unexported. `JSONLStore` gets this right because it is inside package `agent`
   and writes an explicit meta record (`agent/store.go:56`) — the store being
   deleted is the one that is correct. No test caught the difference because the
   shared suite compares sessions whose name the store never persisted.
3. **`List` reads usage through `json_extract(session_json, '$.Usage')`** — a
   path string that depends on Go field names surviving refactors, and on the
   fields staying exported. It broke once already, in the same way as (2).

(2) is the finding worth generalising: `SessionStore` is currently only
*correctly* implementable from inside package `agent`. An out-of-tree store is
the whole point of a two-method interface, so that is an API defect, and it is
what the one exported addition below fixes.

## Public Shape

`SessionStore` does not change. Append-only storage is a backend implementation
choice, not a new contract:

```go
// package agent
type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
}
```

`Save` stays "here is the session as it now stands"; the store works out what is
new. The agent loop keeps calling it after each model message and after each
batch of tool results (`agent/run.go`) — unchanged, and no `Save` per append is
needed. `DiscardStore` stays two trivial methods and stays the default installed
by `agent.New`. `SessionLister` stays the optional capability it is.

### The one exported addition

A store outside package `agent` cannot see `Session.name`, so it cannot restore
one. Give the store seam a name:

```go
// package agent

// SessionRecord is a session as a store sees it: the fields that survive a
// process, and nothing that cannot. A live sandbox handle is not one of them.
type SessionRecord struct {
	ID       string
	Model    string
	Status   Status
	Usage    fantasy.Usage
	Sandbox  string // the sandbox's name; a handle is never persisted
	Messages []fantasy.Message
}

func (s *Session) Record() SessionRecord
func (r SessionRecord) Session() *Session // unbound: Bind before running a turn
```

Two methods and a struct, and stores stop reaching into `Session` in either
direction. Applications never see `SessionRecord` — no example gets longer. The
next field that has to survive a restart is then a one-place change instead of a
per-backend one.

The cheaper alternative, named out loud: replace `SandboxName() string` with an
exported `SandboxName string` field, and `json.Marshal` round-trips it with no
new type at all. Rejected because it demotes an enforced invariant to a
documented one — today the only way a name changes is `Bind`, which returns
`ErrSandboxMismatch` rather than resuming a task in a different filesystem
(`agent/session.go:66`). An exported field lets a caller reassign the name under
a live handle, and rule 4 of `docs/api-design.md` is about not creating that
choice.

## Schema

Replaces migration 1 outright. Nothing depends on the blob table: the store is
one commit old, does not build, and the only file it ever wrote is the
gitignored `metaharness.db` that step 1 deletes. A blob-to-rows migration would
be code written to move data that does not exist.

```sql
CREATE TABLE agent_sessions (
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
);

CREATE INDEX agent_sessions_updated_at ON agent_sessions(updated_at DESC);

CREATE TABLE agent_messages (
    session_id   TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content_json TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (session_id, seq)
);
```

**`sandbox` holds the name and nothing else.** An image, a backend, a daemon
address are this process's configuration, and the process that resumes the
session is free to differ on all of them — the name is the only part that still
means the same thing. That reasoning is written on the JSONL store today
(`agent/store.go:58`) and should move onto this column and onto
`SessionRecord.Sandbox` when that store goes, because it is the argument for
storing a name and not a spec. It makes little
practical difference today, and it is the field that answers "where did this
transcript actually happen" later, so it goes in now while it is free.

**Usage is six columns, not JSON.** It deletes the fragile `json_extract` path,
`List` scans integers instead of decoding a document per row, and "how many
tokens did this agent burn this month" becomes `SUM(...)` instead of a program.
If `fantasy.Usage` grows a bucket, that is one migration string, not a redesign.

**No `message_count` column.** It was denormalized to spare `List` from decoding
every blob to count an array; with a message table the count has a single source
of truth and `List` reads it with a correlated subquery. One indexed count per
listed row on a personal agent's file is not a cost worth a second copy of the
truth.

**No foreign key.** Turso's PRAGMA support is partial and SQLite needs
`foreign_keys=ON` to enforce one anyway, so a `REFERENCES` clause here would be
decoration that reads like a guarantee. If a delete path ever exists, it deletes
messages explicitly, in the same transaction.

### Normalize the transcript, not the message

`content_json` is one whole `fantasy.Message`. Exploding `MessagePart` into its
own table means owning fantasy's part polymorphism — text, reasoning, tool call,
tool result, each output variant — and re-owning it every time fantasy adds a
kind. `fantasy.Message` already round-trips through JSON on its own — the
mechanism the JSONL store depended on for its whole life, asserted by the shared
suite down to a `ToolResultOutputContentError`.

`role` is lifted out into a column because it is the one field worth filtering
and reading at a glance, and because a hand-inspected database should be legible
without piping every row through `jq`. It is a knowing copy of a field inside
`content_json`: it is written *from* the message and never read back into a
`Session` — `Load` takes the role from `content_json` like everything else — so
it is derived, never authoritative, and cannot diverge from the loaded
transcript. That is the difference between this column and the `message_count`
one below it, which would have been a second source of truth.

### Verified, not assumed

Every non-obvious claim above was run against `tursogo v0.7.1` on `:memory:`
before being written down:

- composite `PRIMARY KEY (session_id, seq)` and column defaults: accepted.
- `BEGIN` / upsert / multi-row insert / `COMMIT` through `database/sql`: works.
- `ON CONFLICT(id) DO UPDATE` with a partial column list leaves `created_at`
  untouched while `updated_at` moves.
- a duplicate `(session_id, seq)` fails loudly:
  `turso: constraint failed: UNIQUE constraint failed: agent_messages.(session_id, seq) (19)`.
- the correlated `(SELECT COUNT(*) …)` subquery for `List`: works.
- a 1.3 MB `TEXT` value — the size the suite's large-tool-result case uses —
  round-trips byte-identical.

Turso is a from-scratch reimplementation, not a fork, so compatibility is a
status report rather than a guarantee. Re-run these when the pin moves.

## How Save appends

```go
func (s *Store) Save(ctx context.Context, sess *agent.Session) error {
	rec := sess.Record()
	// how many of this session's messages the database already holds
	stored := s.storedCount(ctx, rec.ID)   // SELECT COUNT(*) … WHERE session_id = ?
	tx := s.db.BeginTx(ctx, nil)
	defer tx.Rollback()
	upsertSession(ctx, tx, rec, s.now())          // status, usage, sandbox, updated_at
	for i := stored; i < len(rec.Messages); i++ { // usually one or two rows
		insertMessage(ctx, tx, rec.ID, i, rec.Messages[i], s.now())
	}
	return tx.Commit()
}
```

`seq` is the message's index in `Session.Messages`, so the transcript's order is
the database's order and `ORDER BY seq` is the whole read path.

The append cursor is a `SELECT COUNT(*)` per `Save`, deliberately: it holds no
state, so it is correct immediately after a `Load`, after a process restart, and
for a `Store` serving many sessions at once — no map, no mutex, nothing to get
out of step. A cached per-session cursor would save one indexed count on a local
file; that is an optimisation to make when something measures it, and the
`(session_id, seq)` key means a stale cursor could only ever fail loudly.

One transaction per `Save` means a crash mid-turn leaves a session row and a
transcript that agree.

`Save` is the only entry point — no separate `Create`. The session row is an
upsert whose `DO UPDATE` list omits `created_at`, so the first `Save` records
when the session started and later ones only move `updated_at`.

`Load` is two queries: the session row, then `SELECT role, content_json FROM
agent_messages WHERE session_id = ? ORDER BY seq`. It assembles a
`SessionRecord` and returns `rec.Session()` — a session with its recorded
sandbox name and no live handle, which is exactly what the resume path needs.

### The transcript is append-only

This design assumes messages are appended and never rewritten in place, which is
what `agent/run.go` does today. Context compaction is the feature that breaks the
assumption, and it needs a decision when it lands rather than a guess now: most
likely a new session row carrying a parent link, so the pre-compaction transcript
stays readable, rather than mutating rows the loop has already persisted.

## Single writer

A session is written by one agent instance. The rules for enforcing that come
later; what the schema gives for free today is worth naming: because `seq` comes
from the stored count and `(session_id, seq)` is unique, a second writer does not
interleave a transcript — it gets a constraint failure on its first append. That
is why the insert is a plain `INSERT` and not `INSERT OR IGNORE`. Losing a write
loudly beats corrupting a transcript quietly.

When enforcement is designed, the hook is a column on `agent_sessions` — a
writer id or a lease with an expiry, checked in the same transaction as the
append. Not now.

## One store, not two: JSONLStore is deleted

`agent.JSONLStore` and `agent.NewJSONLStore` go, along with
`agent/store_jsonl_test.go`, `agent/store_test.go`, and the
`sandboxName.UnmarshalJSON` shim that exists only to read a session shape nothing
has written since `d07dca5`.

Not because JSONL is hard to support. It already carries session metadata — line
1 is a meta record with id, model, status, usage and the sandbox name
(`agent/store.go:56`) — and it is the store that gets the sandbox *right* today.
Append-only is its native shape, too: the one awkward part is that the meta line
is line 1 while metadata changes every turn, and appending meta records with
last-one-wins on read would fix that and make it a pure append log, simpler than
the temp-file-and-rename it does now.

It goes because it is a second file format carrying a second copy of every
decision here — seq, ordering, what is metadata, what a partial write leaves
behind — to serve a fallback role that something else serves better. The Turso
file format is SQLite-compatible: if a pre-1.0 dependency disappoints, the escape
hatch is `modernc.org/sqlite` behind the same schema and the same SQL, which is a
driver swap rather than a format. Two backends also make the shared suite a
lowest-common-denominator constraint on a schema only one of them has.

What the deletion has to preserve:

- **The resumption test.** `agent/session_test.go:148` is Load → `SandboxName()`
  → `Bind` → run, and it reaches for `JSONLStore` only because
  `testutils.MemStore` retains nothing. Make `MemStore` a real in-memory store
  over a `map[string]SessionRecord` — about fifteen lines — and the test stops
  caring which backend exists. That also keeps a second implementation running
  the conformance suite, so the interface cannot quietly become "whatever Turso
  does", without a second format on disk.
- **Readable transcripts.** A real JSONL virtue. The replacement is a `sqlite3`
  query, and a small dump command if that turns out not to be enough — an output
  format, not a storage backend.

There are no production callers: `NewJSONLStore` appears only in tests plus two
lines of prose (`STACK.md:46`, `examples/code-review/README.md:133`). The
exported surface shrinks, so no example gets longer.

Afterwards `agent/store.go` is the interface, `ErrNotFound`, `SessionInfo` and
`SessionRecord` — package `agent` holds no persistence implementation except
`DiscardStore`, which makes "the core is storage-agnostic" literally true rather
than aspirational.

## Error Semantics

- A missing session row returns `agent.ErrNotFound`. A session row with no
  messages is a real, empty session and loads as one — absence of messages is
  not absence of a session.
- Context cancellation propagates; every query takes `ctx`.
- Malformed `content_json` returns a decode error naming the session and the
  `seq`, never a panic and never a silently truncated transcript.
- Wrap with `fmt.Errorf("turso: …: %w", err)` as the store already does.

## Migration Approach

Keep the existing mechanism: `migrations` as an ordered slice of version plus
statements, each applied in a transaction, recorded in
`agent_schema_migrations` (`agentdb/turso/migrate.go`). It is idempotent and
already tested. Migration 1's statements are replaced with the two-table schema
above. Reconsider a migration library after several non-trivial versions.

## Testing Plan

Strict TDD, and the first test is the one that already fails: **the sandbox name
survives a round trip.** It is the defect from "What is in the tree today" stated
as an assertion, and Turso fails it today.

Shared suite (`testutils.RunSessionStoreSuite`, run against the Turso store on
`:memory:` and the in-memory `MemStore`) — behaviour only:

- round trip preserving transcript, usage, status, and sandbox name.
- repeated `Save` on one id: the loaded transcript matches the session, with no
  duplicated or dropped messages. This is how append-vs-rewrite is checked
  behaviourally — the suite must not count rows, because which rows exist is the
  Turso store's business.
- two sessions saved to one store do not mix transcripts.
- a session with no messages loads as a session with no messages.
- `Load` of an unknown id returns `agent.ErrNotFound`.
- a large tool result survives (already present; 1.3 MB, verified against the
  driver).
- a cancelled context propagates from both `Save` and `Load`.
- `SessionLister`: `updated_at` ordering, `limit` respected, `Messages` matching
  the decoded transcript.

`MemStore` earns its place in the suite by being the implementation with no
storage at all: anything the suite asserts that it cannot satisfy is an assertion
about Turso's schema that has leaked into the interface's contract.

Turso-specific tests, where the schema *is* the contract — one of this store's
promises is that a session database is inspectable by hand with `sqlite3`:

- one row per message, `seq` dense from zero, `role` matching the message.
- a message inserted at an existing `seq` fails rather than duplicating.
- `created_at` set on first `Save`, unchanged by the second; `updated_at` moves.
- usage columns readable as integers, and summable across sessions.
- `Migrate` twice on one database: no error, one row per version (exists).
- malformed `content_json` produces a decode error (rewrite of the existing
  malformed-blob test).
- reopening a file-backed store loads what the closed one saved (exists).

The Turso suite is unskipped and runs in normal `make test` — `:memory:` plus no
CGO means there is nothing to gate.

## Rollout Steps

1. Delete the gitignored `metaharness.db` from the repo root.
2. Add `agent.SessionRecord`, `Session.Record()` and `SessionRecord.Session()`.
   Fix `testutils/session_store.go` onto the sandbox name so the tree builds
   again, and add the sandbox-name assertion to the shared suite — it fails on
   Turso, which is where step 4 starts.
3. Delete `JSONLStore`: the type, `NewJSONLStore`, the `sandboxName` shim,
   `agent/store_jsonl_test.go` and `agent/store_test.go`. Give `MemStore` real
   retention and move `agent/session_test.go`'s resumption test onto it. Update
   `STACK.md:46` and `examples/code-review/README.md:133`, which are the only
   prose that names the store.
4. Replace Turso migration 1 with the two-table schema; rewrite `Save` as one
   transaction (upsert plus the new-message tail), `Load` as two queries, and
   `List` onto the usage columns and the correlated count. The
   `json_extract(session_json, …)` path goes away with it.
5. Extend the shared suite and add the Turso-specific schema tests.
6. Wire the store into `examples/telegram-chat` and add `/sessions` and
   `/resume <id>` to the bridge. This is the persistence gap that is actually
   felt today, and `/resume` is what exercises `Load` plus `Bind` against a live
   sandbox — the pair no unit test can prove works. It is also where the store
   stops being opt-in-and-unused: with JSONL gone, an application that wants
   persistence at all links the driver, and the example should show that being a
   choice rather than a default.
7. Update `STACK.md` again for the shape: the layout list is right, the one-line
   description of `agentdb/turso` is not once it stores messages, and the
   "persistence is opt-in" paragraph names a store that no longer exists.

## Later, deliberately not now

- **Write leases.** See "Single writer".
- **Compaction.** See "The transcript is append-only".
- **Memory.** Its own schema and its own narrow interface, defined where memory
  is used, when the runtime has a concrete memory behaviour to call. One
  constraint is already known and worth keeping: **Turso has no FTS5.** It is
  explicitly unsupported and Turso ships its own full-text search with different
  syntax, so retrieval cannot be "add an FTS5 virtual table" and stay portable.
  Look up the current syntax when retrieval lands, keep every query behind the
  interface, and note that a `LIKE` scan over a personal agent's memories is a
  perfectly good first implementation.
- **A filesystem backend** (`agentdb/file`): one folder per agent, Markdown
  memories with front matter. Only when memory needs a folder-backed
  implementation. Sessions already have their folder-backed store.
