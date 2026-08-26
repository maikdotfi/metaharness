# Key-Value Store Plan

**Landed.** Two things came out different from the design below, and both are
recorded where the decision was made: `SessionLister.List` had to move out of the
way (see "The `List` collision"), and the prefix successor is computed by rune
rather than by byte (see "Range bounds, not `LIKE`").

## Direction

`MEMORY-PLAN.md` left the application's own durable state to the application:

> The podcast agent's episode ledger is the application's, not the library's — it
> is a schema with opinions about someone else's domain, and one application is
> not evidence.

That is still right about the schema and wrong about the conclusion. A podcast
episode ledger has no business in this library. But the step from "the library
must not hold a podcast schema" to "the application therefore writes DDL" skips a
possibility: the library can hold a place to put a ledger without holding the
ledger.

Build that place. **One table, no domain in it.** Opaque string keys, opaque
bytes, ordered prefix listing. The library stays ignorant of what an application
stores, and the application stops defining tables in a database it did not
create.

The evidence this is the right shape is already in the tree: `agent_notes` is a
key-value table with one consumer's name on it. `topic` is a key, `content` is a
value, `Replace` is a put, and `memory.Store` is a three-method interface over
it. The library has built this once already.

The evidence it is the right *place* is thinner — one application, the podcast
agent. See "What would falsify this".

## Goals

- Give an application a durable place for its own state, in the database it
  already opened, without a line of DDL anywhere in the application.
- Keep the library ignorant of what is in it. Keys and values are opaque.
- Ordered prefix listing, so an application's key layout can carry sort order.
- Narrow enough that a second backend is a new type, not a change here.
- One more table in the migration list metaharness already owns, so there is one
  schema owner per database.
- Stay inspectable with the `sqlite3` CLI, like everything else in the file.

## Non-Goals

- No buckets, namespaces, or collections. The key is the namespace.
- No codec. Values are bytes; JSON is the application's choice, not the schema's.
- No typed or generic helpers over those bytes.
- No compare-and-swap, no multi-key transactions — see "No CAS".
- No TTL or expiry.
- No querying by value, no secondary indexes, no full-text search. Turso has no
  FTS5, and a value-shaped query would need the library to know the value's
  shape, which is the whole thing being avoided.
- No prefix delete yet.
- No migrating `agent_notes` onto it, and no reworking `memory.Store` — see
  "Later, deliberately not now".

## The caller

```go
store, err := turso.Open(ctx, opt.dbPath)
defer store.Close()

a := agent.New(systemPrompt,
	agent.WithModel(m),
	agent.WithStore(store),
	agent.WithMemory(memory.SystemPrompt(store)),
	agent.WithTools(podcast.Tools(store)...),
)
```

The store arrives a third time, for exactly the reason `MEMORY-PLAN.md` gives for
it arriving a second: one database serving three subsystems, each line a separate
decision the application is entitled to make differently.

What the application no longer has is the alternative `MEMORY-PLAN.md` sketched:

```go
db, err := sql.Open("turso", opt.dbPath)   // gone
turso.Migrate(ctx, db)                     // gone
store := turso.New(db)                     // gone
episodes := podcast.Tools(db)              // now takes the store
```

`turso.Open` goes back to being the one-call path even for an application that
keeps state of its own, and `turso.New` goes back to being for callers who
genuinely own the handle for other reasons.

## Public Shape

### `package agentdb`

A new package in the directory that is currently only a parent. Two
declarations, no implementation.

```go
// Package agentdb declares what an agent database is, independently of which one
// an application chose.

// KV is a durable place for an application's own state, in the database its
// agent already uses. Keys and values are opaque to the store: what a key means
// and what a value decodes to are the application's business.
type KV interface {
	// Get returns the value stored under key. A key that was never written is
	// not an error: found reports whether there was one.
	Get(ctx context.Context, key string) (value []byte, found bool, err error)

	// Put writes value under key, replacing whatever was there.
	Put(ctx context.Context, key string, value []byte) error

	// Delete removes key. Deleting a key that is not there is not an error.
	Delete(ctx context.Context, key string) error

	// List returns every entry whose key starts with prefix, ordered by key.
	// The empty prefix lists everything. Ordering is bytewise, so a key layout
	// that encodes a sortable field sorts by it.
	List(ctx context.Context, prefix string) ([]Entry, error)
}

// Entry is one stored value and what is known about it.
type Entry struct {
	Key     string
	Value   []byte
	Updated time.Time
}
```

`turso.Store` gains the four methods and a `var _ agentdb.KV = (*Store)(nil)`.
Nothing inside metaharness consumes `KV`; it exists for the application and for
the second backend.

One existing method had to move to make room. See "The `List` collision".

## Decisions

### Reversing `MEMORY-PLAN.md`'s "the application's own tables"

The earlier decision rejected the library holding a domain schema. It did not
consider the library holding a schemaless place for one, so the argument it makes
does not reach this proposal: `agent_kv` has no opinion about podcasts, or about
anything.

One claim in the earlier plan should be corrected while overturning it. It said
importing `agentdb/turso` is what registers the driver, so `sql.Open` "needs
nothing further" — and that is true (`sql.Register("turso", …)` in
`turso.tech/database/tursogo/driver_db.go:72`). The application never needed to
name the driver dependency. So the cost of the old path was never an import; it
was the DDL, and specifically:

- two components defining tables in one file, each with its own version
  bookkeeping, and neither able to see the other's;
- a `CREATE TABLE` inside an application that otherwise contains no SQL at all;
- and every future column addition needing a migration mechanism in the
  application, with no `PRAGMA table_info` to fall back on, because Turso's
  PRAGMA support is partial.

Removing DDL from applications is the whole return on this table.

### The `List` collision

`turso.Store` already had a `List(ctx, limit int) ([]agent.SessionInfo, error)`,
satisfying `agent.SessionLister`. One type cannot carry two `List` methods, so
one of them had to be renamed, and the choice is which operation deserves the
bare verb.

The KV keeps it. `List` over an opaque table with opaque keys *is* the generic
operation, and `ListSessions` says exactly what the other one does — a name that
was always better and was only tolerable while nothing competed for it. So
`agent.SessionLister` now declares `ListSessions`, and `turso.Store` implements
that.

The rename is confined to the store-side interface, which nothing outside a
backend implements. `agent.Sessions.List` — the facade an application and a
bridge actually call (`agent/sessions.go`, `bridge/xmpp/bridge.go`'s
`/sessions`) — keeps its name, so no caller of the library changed.

### One table, not one per application

`agent_kv`, shared. Two applications pointed at one database file would collide
on keys, and that is correct: they already collide on `agent_notes` and on
`agent_sessions`. A second agent gets a second file, the way a second sandbox
gets a second name.

### Opaque bytes, and `[]byte` rather than `string`

The library must not know what a value is. `[]byte` says that plainly, marshals
straight out of `encoding/json`, and does not invite the assumption that values
are text. Any codec — JSON, and it will be JSON — is the application's line.

### `Get` returns `found`, not `ErrNotFound`

An absent key is the normal case for a key-value store: "have I seen this
episode?" is a `Get` that is expected to miss. A sentinel error would make every
call site an `errors.Is`. This differs from `agent.ErrNotFound`, which `Resume`
returns because a session id that does not exist is a user's mistake rather than
an ordinary answer.

### Ordered prefix listing is the only query

Everything else is a scan and a filter in Go, in the application. That is not a
performance compromise being waved through: an application storing a document per
thing it tracks has tens or hundreds of them, listed twice a day. When one has
enough state for that to hurt, it has enough state to own a table — which this
table does not prevent.

### Range bounds, not `LIKE`

`List` uses `WHERE key >= :prefix AND key < :successor`, not
`WHERE key LIKE :prefix || '%'`. `_` is a single-character wildcard in `LIKE`,
and `show/foo_bar` is an entirely reasonable key, so the `LIKE` form is wrong for
real inputs unless every call carries an `ESCAPE` clause. The range form also
uses the primary key index without depending on how the query planner feels
about `LIKE`.

The successor of a prefix is the prefix with its **last rune** incremented,
dropping trailing runes that cannot be; a prefix that is empty, or entirely the
highest rune, has no upper bound and the clause is omitted.

By rune and not by byte, which the design got wrong. `key` is a `TEXT` column and
the driver rejects a parameter that is not valid UTF-8, so `"café"` with its last
byte incremented is not a string that can be bound at all. UTF-8 sorts in code
point order, so the rune form and the byte form agree on ordering and only the
arithmetic differs. The increment also has to step over the surrogate range,
which UTF-8 cannot encode.

The consequence for callers: **keys must be valid UTF-8.** Values need not be —
they are a `BLOB`.

### No CAS

The XMPP bridge runs one turn at a time (`bridge/xmpp/bridge.go`, the `serve`
loop), so within an agent there is one writer. Turso's MVCC means a second
process writing a different key is not a correctness problem this schema has to
dodge. `PutIf` arrives when two writers do.

### `KV` and `Entry` live in `agentdb`, not in the consumer

This departs from the pattern `memory.Store` sets, where the consuming package
declares the interface it needs and `turso.Store` happens to satisfy it. That
works because `memory` is in the tree. Here the consumer is an application, out
of tree — so there is no consumer package to hold the declaration, and an
application cannot declare its own `Entry` type either: method sets have to match
exactly, so `List` returning `podcast.Entry` would not satisfy anything
`turso.Store` implements.

Naming the interface once, in a package that names no driver, is what keeps the
application depending on "an agent database" rather than on Turso, and gives a
second backend something to implement. `agentdb` holds two declarations and
imports `context` and `time`.

## Schema

Turso migration 3, one table:

```sql
CREATE TABLE agent_kv (
	key        TEXT PRIMARY KEY,
	value      BLOB NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
```

`BLOB`, because values are bytes and the schema should not imply otherwise. If
`tursogo` disappoints on blob round-tripping, the fallback is `TEXT` with the
application base64ing — and the suite's invalid-UTF-8 case below is the test that
would catch it before anything else does.

No index beyond the primary key: it is what the prefix range scan walks, and it
is what makes `ORDER BY key` free.

`Put` is an upsert whose update list omits `created_at`, the same shape
`writeNote` and `upsertSession` already use, so the row records when a key first
appeared. Timestamps use the existing `timestampFormat` — fixed width, so
lexicographic order is chronological order and an application can put one in a
key.

## Testing Plan

Failing test first. Most of these are backend-shaped, so they go in a shared
suite in `testutils` alongside the `SessionStore` one, and a second backend has
something to pass before it exists:

```go
func KVSuite(t *testing.T, open func(t *testing.T) agentdb.KV)
```

- `Get` on a key never written returns `found == false`, a nil value, and no
  error.
- `Put` then `Get` round-trips the exact bytes, including an empty value and a
  value that is not valid UTF-8.
- `Put` twice on one key replaces the value, keeps `created_at`, and moves
  `Updated`.
- `Delete` removes the key; `Delete` on an absent key is not an error.
- `List("")` returns every entry, ordered by key.
- `List(prefix)` returns only that prefix, ordered, and includes a key exactly
  equal to the prefix.
- `List` over an empty range returns nil and no error.
- **`List` on a prefix containing `_` and `%` treats them literally.** This is
  the `LIKE` regression, and it is the test that makes the range-bound decision
  load-bearing rather than a comment.
- `List` on a prefix ending in the highest encodable rune still bounds
  correctly, as does one ending in the rune just below the surrogate range.
- Ordering is bytewise: a case-mixed key set comes back in byte order, not in a
  collation's order.

And in `agentdb/turso`, against a real file:

- `Migrate` twice leaves one `agent_kv` and no error.
- Reopening a file-backed store reads what the closed one wrote.
- `agent_kv` and `agent_notes` do not interfere: writing a note does not appear
  in `List("")`, and vice versa.

## Rollout Steps

1. Add `package agentdb` with `KV` and `Entry`. No implementation, no consumer.
2. Add migration 3 and the four methods in `agentdb/turso/kv.go`, next to
   `notes.go`, with `var _ agentdb.KV = (*Store)(nil)`.
3. Add `testutils.KVSuite` and run it against the Turso store and against an
   in-memory fake, so the fake is available to application tests.
4. Update `STACK.md`'s layout list and its persistence paragraph, and add a line
   to `README.md`. One sentence each. If either explanation runs longer, the API
   got worse — go back to step 1.
5. Check the callers. `examples/xmpp-chat` grows nothing at all. The application
   that motivated this grows one `WithTools` line and loses a `sql.Open`, a
   `Migrate`, a `turso.New`, and its schema file.

## What would falsify this

If the second application to want durable state wants it *relational* — queries
across its records, an aggregate, a join — then the KV was the wrong
generalisation from one case, and `MEMORY-PLAN.md`'s answer was right: the
application owns a table. Nothing here forecloses that; the KV is an addition,
and an application is still free to open the handle and define its own schema.

The signal to watch for is an application maintaining its own index keys inside
the KV to make a query fast. That is a relational schema being smuggled in
through a key layout, and it means the application should have had a table.

## Later, deliberately not now

- **`memory.Store` on top of `KV`.** They are the same shape, and the
  unification is tempting. What it costs is `Append`'s single-round-trip
  `content || char(10) || ?`, which becomes a read, a concatenation in Go, and a
  write. It is also a shipped API with a live consumer. Not for a tidier tree.
- **`agentdb/file`.** One file per key under a directory, hand-editable and
  greppable — the backend `STORE-PLAN.md` has anticipated twice now. It is more
  attractive for a KV than it ever was for sessions, and it is still a second
  implementation before the first has run.
- **`PutIf` / compare-and-swap.** When there are two writers.
- **Prefix delete.** `Delete` in a loop is fine for now, and a `DeletePrefix`
  that quietly removes a hundred keys wants more thought than this needs.
- **A value size limit.** Worth having when something large gets put in here;
  the podcast agent deliberately keeps transcripts on disk and stores a path.
