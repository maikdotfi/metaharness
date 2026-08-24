# Memory Plan

## Direction

`STORE-PLAN.md` deferred memory until "the runtime has a concrete memory
behaviour to call." A twice-daily podcast agent that reports over XMPP and
learns from the replies is that behaviour, and it makes the requirement precise:
what the user says about a summary at 07:40 has to still be true next week, in a
session that does not exist yet.

Memory is therefore **not** the transcript. A session is one bounded task and
stays disposable; memory is the thin durable line between sessions. That
division is what lets `SCHEDULE-PLAN.md` throw a session away twice a day
without the agent losing what it knows.

Build one kind of memory: everything the agent knows, rendered into the system
prompt, with one tool to write it. It is the simplest thing that works, and it
is enough for a personal agent whose notes are a page of prose. Other kinds —
retrieval, per-topic recall, summarised history — are real and are expected, but
they arrive when an agent cannot hold its notes in a prompt, not before.

Two constraints are inherited and still hold. **Turso has no FTS5**, so
retrieval, when it lands, is not "add a virtual table". And a `LIKE` scan over
one person's notes is a perfectly good first implementation.

## Goals

- Give an agent notes that outlive its sessions, in one option.
- Make recall arrive automatically. The agent must not have to remember to ask.
- Give the model exactly one thing it can do to memory: write a note.
- Keep the storage seam narrow enough that a second backend is a new type, not a
  change to `memory`.
- Keep the memory *kind* a seam too: how notes reach the model is a choice, and
  the interface should survive a second answer to it.
- Keep `package agent` free of any storage implementation, as it is for sessions.

## Non-Goals

- No retrieval, ranking, embedding, or search. One agent, one page of notes.
- No second memory kind alongside the first.
- No memory schema in `package agent`.
- No automatic extraction of memories from the transcript. The model decides
  what is worth keeping, by calling a tool, the way a person decides.
- No deletion yet — see "Later, deliberately not now".
- No sharing of memory between agents or users. This is a 1:1 personal agent.

## The caller

```go
store, err := turso.Open(ctx, opt.dbPath)
defer store.Close()

a := agent.New(systemPrompt,
	agent.WithModel(m),
	agent.WithStore(store),
	agent.WithMemory(memory.SystemPrompt(store)),
)
```

One line per subsystem, and the line names the choice being made: notes live in
the same database as the sessions, and they reach the model through the system
prompt. Drop the line and the agent forgets between sessions; nothing else in
the assembly changes.

There is deliberately no second line registering a memory tool. See "One option,
not an option and a registration".

## Public Shape

### `agent.Memory`

The interface is declared in `package agent`, for the same reason
`SandboxOpener` is (`agent/sandbox.go:38`): it is the one thing the agent needs
from whoever owns the memory, and declaring it here points the dependency one
way. `package memory` imports `agent`; `agent` imports nothing new.

```go
// package agent

// Memory is what an agent knows between sessions. The kind of memory decides
// both how it reaches the model and what the model can do to it, so an
// implementation supplies its own tools rather than the application wiring
// them.
type Memory interface {
	// Recall returns text for the system prompt, or "" for nothing to add. It
	// is called once at the start of a turn, not once per model call.
	Recall(ctx context.Context) (string, error)

	// Tools are the memory's own tools. WithMemory registers them.
	Tools() []Tool
}

func WithMemory(m Memory) Option
```

`Recall` takes no session. A memory kind that picks notes by relevance needs
one, and when that kind is built the signature grows a parameter — a one-line
change to an interface with two implementations. Adding it now would be a
parameter no implementation reads, which is the same speculation
`BRIDGE-PLAN.md` refused for a transport interface.

### `package memory`

```go
// SystemPrompt returns a memory that renders every note into the system prompt
// and gives the model one tool, remember, to write them. It suits an agent
// whose notes stay a page long; nothing prunes them.
func SystemPrompt(s Store) agent.Memory

// Store is where notes live. One entry per topic, newest write wins.
type Store interface {
	Notes(ctx context.Context) ([]Note, error)
	Append(ctx context.Context, topic, line string) error
	Replace(ctx context.Context, topic, content string) error
}

// Note is one topic's durable content.
type Note struct {
	Topic   string
	Content string
	Updated time.Time
}
```

`SystemPrompt`, not `SystemPromptMemory`: the package qualifies it already.

`Append` and `Replace` are two methods rather than one `Write` with a mode,
because a mode argument is a second thing the call site has to be read against.
They are also the two operations the model is offered, so the store's vocabulary
and the tool's are the same words.

### The `remember` tool

```go
type rememberArgs struct {
	Topic   string `json:"topic" description:"A short stable slug: taste, skip-rules, summary-style."`
	Content string `json:"content" description:"One or two sentences, in the user's own words."`
	Replace bool   `json:"replace,omitempty" description:"Replace the topic's note instead of adding a line to it."`
}
```

The tool is unexported and reached only through `SystemPrompt`. There is nothing
an application would do with it that `WithMemory` does not already do, and
exporting it would make "which of these two do I need?" a question the docs have
to answer.

**The discipline lives in the tool description, not in a skill.** Write it down
the moment it is stated; do not record a one-off instruction, anything the user
asked you to forget, or a guess. That rule only works if it is in front of the
model on the turn the user says the thing, and a skill the model has to decide
to load is not. A bundled skill would be a second copy of the same words.

### What recall renders

```text
What you remember about the person you work for. These notes came from things
they told you in earlier conversations; treat them as current unless they say
otherwise.

## taste
Prefer deep technical dives. Hard no on anything over three hours.

## summary-style
Lead with the argument, not the guest list.
```

Appended to the application's system prompt. With no notes, **nothing is
appended** — no empty block, no "you have no memories yet". The common case has
no line, and a paragraph explaining an absence is prompt the model has to read
every turn.

## Decisions

### Recall once per turn, not once per model call

`Run` passes `a.SystemPrompt` on every model call inside its loop
(`agent/run.go:80`). Recall is resolved **once, before the goroutine starts**,
and the resolved text is used for the whole turn. So a `remember` call takes
effect on the *next* turn, which is a rule that can be stated in one sentence.
The alternative — recomputing per call — changes the system prompt in the middle
of a turn, breaks the prompt cache on every tool round trip, and makes the
model's own write visible to it mid-reasoning in a way nothing else in the loop
is.

A failing `Recall` fails the turn, before it starts: `Run` already returns
`(<-chan Event, error)`, and the bridge already reports a turn that would not
start. Silent amnesia is the worse failure — the agent would answer confidently
without the one thing that makes its answers personal.

### One option, not an option and a registration

The application must not hand the same store to two constructors. That was the
defect in the Telegram `SessionFactory` (`docs/api-design.md`, move 8): the
manager arriving a second time. So `WithMemory` registers the memory's tools
itself, and `Memory.Tools()` exists for that and nothing else.

It follows that a memory kind is free to offer more than one tool — a retrieval
memory would offer a search tool too — without any application changing.

### No read tool

Recall is injected, never fetched. The podcast-digest skill puts it best:
memory should arrive automatically rather than on remembering to ask. A model
that has to call `recall` before it can be personal will sometimes not, and the
failure is invisible — a competent, generic answer.

### The store arrives twice, deliberately

`agent.WithStore(store)` and `agent.WithMemory(memory.SystemPrompt(store))` name
the same value. That is not the "value held only to surrender" smell: it is one
database serving two subsystems, and each line is a separate decision the
application is entitled to make differently. Sessions in a file and memory
nowhere is a legitimate build; so is the reverse. Detecting a memory capability
on the session store by type assertion would remove the second line and the
choice with it.

### The application's own tools

The podcast agent's episode ledger is the application's, not the library's — it
is a schema with opinions about someone else's domain, and one application is
not evidence. Its tools take whatever store the application already has, which
is the shape the memory store uses too.

An application that wants its episodes in the *same* file as its sessions and
notes needs no new API. It opens the database itself and hands it over:

```go
db, err := sql.Open("turso", opt.dbPath)
turso.Migrate(ctx, db)
store := turso.New(db)
episodes := podcast.Tools(db)
```

`turso.New` and `Migrate` are already the seam for this
(`agentdb/turso/store.go:84`), and importing `agentdb/turso` is what registers
the `turso` driver, so `sql.Open` needs nothing further. This is the
`database/sql` convention rather than a leak — the same move as the
`_ "sandbox/docker"` line in `examples/telegram-chat`, where the import *is* the
choice. An application that reaches for this store has already chosen turso;
naming its driver says nothing new about it, and choosing a different one is the
same single line.

`turso.Open` stays the one-call path for an application with nothing else in the
file, and which of the two an example uses changes one line.

## Schema

Turso migration 2, one table:

```sql
CREATE TABLE agent_notes (
	topic      TEXT PRIMARY KEY,
	content    TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
```

No agent id column. This is a personal agent and the database is its; a second
agent gets a second file, the way a second sandbox gets a second name.

`Append` is `UPDATE agent_notes SET content = content || char(10) || ?` with an
insert fallback: one statement rather than a read-modify-write round trip in Go.
`Replace` is an upsert whose update list omits `created_at` — the same shape
`upsertSession` already uses. Neither needs a concurrency argument to justify it;
Turso's MVCC means a second writer is not a problem this schema has to dodge.

## Testing Plan

Failing test first, and the first ones are caller-shaped.

`package agent`:

- an agent with memory calls `Recall` once for a turn that makes three model
  calls, and every call carries the same system prompt.
- the recalled text appears in `ModelRequest.System` after the application's
  prompt.
- `Recall` returning "" leaves the system prompt byte-identical to no memory.
- `Recall` returning an error makes `Run` return it, and no model call happens.
- `WithMemory` registers the memory's tools; a name collision with an
  application tool panics like any other duplicate.
- an agent without memory behaves exactly as today (the regression guard).

`package memory`, against a fake `Store` and against the real Turso one through
a shared suite, the way `testutils` already does for `SessionStore`:

- `remember` with `replace: false` on a new topic creates it; on an existing one
  it adds a line and keeps what was there.
- `remember` with `replace: true` overwrites.
- notes render newest-topic-ordering-independent: the rendered block is stable
  for a given set of notes, so a test can assert on it and a prompt cache can
  hit.
- a note written in one turn is recalled in the next (this is the behaviour the
  whole plan exists for, and it belongs in an end-to-end test with a fake model).

`agentdb/turso`:

- `Migrate` twice leaves one `agent_notes` schema and no error.
- `Append` twice on one topic keeps both lines, in order.
- reopening a file-backed store recalls what the closed one wrote.

## Rollout Steps

1. Add `agent.Memory` and `agent.WithMemory` with the tests above. No
   implementation yet; `DiscardStore`'s counterpart is simply a nil `Memory`.
2. Add `package memory`: `Store`, `Note`, `SystemPrompt`, the `remember` tool,
   and the rendering. Test against a fake store.
3. Add Turso migration 2 and the three `Store` methods on `turso.Store`. Add the
   shared store suite so a second backend has something to pass.
4. Wire it into an example and check the diff: the assembly grows one line.
5. Update `STACK.md`'s layout list and its persistence paragraph, and add the
   memory line to `README.md`. If either explanation gets longer than a
   sentence, the API got worse — go back to step 1.

## Later, deliberately not now

- **Forgetting.** `Replace` with empty content deleting the row is a hidden rule,
  which is documentation standing in for API. A `Forget(ctx, topic)` method and a
  `forget` tool land together, when a wrong note actually costs something.
- **A second memory kind.** Retrieval is the obvious one, and it is what makes
  `Recall` grow a session parameter. Build it when an agent's notes stop fitting
  in a prompt, and let the concrete case pick the signature.
- **`agentdb/file`.** One folder per agent, Markdown notes with front matter,
  hand-editable and greppable — the backend `STORE-PLAN.md` already anticipated.
  It is more attractive for memory than it ever was for sessions, and it is still
  a second implementation before the first has run.
- **Pruning or summarising notes.** `SystemPrompt` grows without bound by design.
  The bound that matters is the user's attention, and they can read the file.
