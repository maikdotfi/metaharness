# Personal Telegram Bridge Plan

## Direction

The first bridge is a personal Telegram bot for talking to one assembled agent.
It uses Telegram long polling, exposes no HTTP server, and keeps one current
agent session in memory.

Telegram identifies where messages arrive and replies are sent. It does not
identify an agent session. A session represents one bounded task context and
gets a fresh opaque ID whenever the user starts a new task.

Build the Telegram integration directly. Do not introduce a generic bridge or
transport interface until a second bridge provides a concrete comparison.

## Goals

- Make an assembled Meta Harness agent usable from a personal Telegram chat.
- Receive updates through long polling only; never require a public listener.
- Support multi-turn conversation within the current task.
- Let the user discard the current context and start a fresh task.
- Keep persistence optional and run the initial bridge entirely in memory.
- Restrict access to explicitly configured Telegram user IDs.
- Keep Telegram-specific types and dependencies out of the core `agent` package.
- Shut down cleanly through `context.Context`.

## Non-Goals

- No generic `Bridge`, `Transport`, or `Conversation` interface yet.
- No webhooks or inbound HTTP server.
- No groups, channels, forum topics, media, reactions, or inline interactions.
- No persisted conversation history or restoration after process restart.
- No switching between or returning to old sessions.
- No immediate cancellation of an in-flight agent turn.
- No proactive notifications from autonomous/background agent runs yet.
- No exactly-once delivery guarantee across process crashes.

## Structure

Proposed layout:

```text
agent/
  store.go                  SessionStore, DiscardStore, JSONLStore

bridge/
  telegram/
    bot.go                  polling, authorization, commands, agent turns, progress delivery
    text.go                 assistant text extraction, message splitting, stepLine rendering
    bot_test.go
    text_test.go

examples/
  telegram-chat/
    main.go                 assembles one concrete personal agent
    README.md               BotFather and environment setup
```

The reusable package is specifically `bridge/telegram`, not a bridge framework.
The example owns model, tools, sandbox, prompt, and other agent-specific wiring.

## Optional Persistence

`Agent.Run` currently requires a non-nil `SessionStore`, even though the caller
owns the live `*Session`. Add a production discard implementation:

```go
// DiscardStore accepts session checkpoints without retaining them.
type DiscardStore struct{}

func (DiscardStore) Save(context.Context, *Session) error { return nil }
func (DiscardStore) Load(context.Context, string) (*Session, error) {
    return nil, ErrNotFound
}
```

Make `DiscardStore` the default installed by `agent.New`. Applications opt into
persistence with `agent.WithStore`; existing JSONL behavior remains unchanged.

Besides making the personal bridge storage-free, `DiscardStore` is useful in
tests that exercise the agent loop but do not care about checkpoints. Keep the
existing recording `testutils.MemStore` for tests that need to assert saves.

## Telegram Package Shape

Use `github.com/go-telegram/bot` for Bot API types, long polling, request
handling, and Telegram error handling. Confine the dependency to
`bridge/telegram`.

A small initial configuration is enough:

```go
package telegram

type SessionFactory func() *agent.Session

type Config struct {
    Token        string
    Agent        *agent.Agent
    NewSession   SessionFactory
    AllowedUsers []int64

    // ShowThinking includes the model's raw reasoning text in the progress
    // status message. Progress itself is always reported and is not optional;
    // this flag only chooses between exposing the reasoning text and showing a
    // bare "thinking…" step. There is deliberately no flag to disable progress.
    ShowThinking bool
}

func Run(ctx context.Context, cfg Config) error
```

`NewSession` keeps model and sandbox selection in the assembling application.
It returns a session with a fresh opaque ID, the chosen model, an active status,
and any desired sandbox specification.

The bot owns the current session:

```go
type personalBot struct {
    agent      *agent.Agent
    current    *agent.Session
    newSession SessionFactory
    // Telegram client, authorization data, and serialization follow.
}
```

There is deliberately no Telegram chat-to-session mapping in this version.
The allowlisted personal user talks to whichever session is current.

## Session Lifecycle

At process startup, call `NewSession` once. A normal text message:

1. Passes the authorization and private-chat checks.
2. Is appended to `current.Messages` as a Fantasy user message.
3. Sets `current.Status` to `active`.
4. Runs the agent and consumes its `Event` channel to completion, reporting
   progress (see Progress) as `EventAssistant` and `EventToolResult` arrive.
5. Delivers the reply from the terminal `EventDone` message only. `EventAssistant`
   and `EventToolResult` feed progress and are never sent as the answer. This
   matters because the final assistant text arrives twice — once as an
   `EventAssistant` (text, no tool calls) and again inside `EventDone` — so
   delivering solely on `EventDone` avoids double-sending.
6. Sends that final text back to the same Telegram chat as a fresh message.

Starting a new task replaces the current session by calling `NewSession` again.
The previous session becomes unreachable and is eventually garbage collected.
Restarting the process also starts a fresh session by design.

Session IDs are opaque local identifiers such as `sess_<random>`. They must not
encode a Telegram bot, chat, or user ID.

## Progress

Progress reporting is mandatory, not a configuration option. Without it a
tool-looping turn looks broken or stuck, which is unacceptable for a personal
chat interface. The bridge always keeps the user informed while a turn runs.

Two layers work together:

- The `typing` chat action (see Telegram Behavior) provides continuous liveness.
- A single status message provides step-level progress. On turn start the bridge
  sends one placeholder message and then edits it as `Event`s arrive, rather than
  posting a new message per step, which would spam the chat. A typical trail:

```text
💭 thinking…
🔧 bash: ls -la
🔧 read_file: agent/run.go
✅ done
```

When the turn completes, the final answer is sent as a fresh message and the
status message is left as a compact trail (or cleared).

Because the model call is non-streaming, progress granularity is per model call
and per tool result — `EventAssistant` (reasoning and/or tool calls) and
`EventToolResult`. That is the right granularity for Telegram anyway, whose
`editMessageText` rate limits force any finer token stream to be re-batched.

`ShowThinking` controls only whether the model's raw reasoning text is included
in the status message. When it is off, a reasoning turn still shows a bare
`💭 thinking…` step, so progress is present either way; the flag never disables
progress.

Rendering one event to one progress line is a pure, transport-neutral function
and is the only reusable piece extracted here:

```go
// stepLine renders one agent event as a short progress line, plus whether it
// should be shown. It has no Telegram dependency, so a future transport can
// reuse it directly. It does NOT imply a generic bridge or transport interface.
func stepLine(ev agent.Event, showThinking bool) (line string, ok bool)
```

Delivering that line — placeholder creation, edit throttling, final cleanup — is
Telegram-specific and stays in `bot.go`. Only `stepLine` is extracted for reuse.

## Commands

Initial commands:

- `/new` discards the current conversational context and starts a fresh session.
- `/clear` is an alias for `/new`.
- `/status` reports the current session ID, model, message count, and token usage.
- `/help` and `/start` explain the available commands.

Resetting creates a new session, so messages, usage, status, and session ID are
all reset together. It does not destroy, clear, or replace a sandbox. Chat
context and workspace lifecycle are separate concepts.

Commands are handled by the bridge and never appended to the agent transcript.

## Turn Serialization

The initial personal bot processes one update at a time. A global mutex or a
single Telegram handler worker is sufficient and prevents concurrent mutation
of `Session.Messages` while `Agent.Run` is using it.

Messages received during an agent turn wait in order. Consequently, `/new`
received during a turn takes effect after that turn completes. Immediate
cancellation can be added later by retaining a `context.CancelFunc` for the
active run and defining the transcript semantics of an interrupted turn.

Do not add keyed locks or per-chat workers until the bridge intentionally grows
beyond one personal user.

## Telegram Behavior

- Request only `message` updates.
- Accept text messages from private chats only.
- Ignore unsupported update and message types.
- Use plain text for replies initially; avoid Telegram Markdown escaping issues.
- Send a `typing` chat action as soon as a turn begins and refresh it on a ~4s
  ticker for the duration of the agent turn, so the chat never looks stuck.
  Liveness is not streaming: the action is ephemeral, adds nothing to the
  transcript, and stops when the turn ends.
- Split long replies into chunks within Telegram's message length limit,
  preserving UTF-8 boundaries.
- The delivered answer is only the final assistant text. Tool calls and tool
  results stay in the agent transcript and logs; they reach the user only
  through the transient progress status message (see Progress), never as the
  answer.
- If the final assistant message has no text, send a short explicit fallback.
- Propagate shutdown cancellation into both polling and the active agent run.

Long polling and webhooks are mutually exclusive. The setup documentation must
explain how to remove an existing webhook before running the bridge. It must
also make the queued-update policy explicit: retaining pending updates replays
messages after downtime, while dropping them avoids unexpectedly running stale
prompts.

For the personal first cut, prefer dropping pending updates at startup. Durable
inbox processing can be designed later if offline delivery becomes desirable.

## Security

An agent with file and shell tools is a privileged interface. The bridge must:

- Deny every sender not present in `AllowedUsers`.
- Authorize with the immutable numeric Telegram user ID, not username.
- Require at least one allowed user during configuration.
- Keep the bot token in an environment variable and never log it.
- Avoid echoing raw Telegram updates at normal log levels.
- Treat message text as untrusted agent input, just like any other user prompt.

Unauthorized messages should be ignored or answered with a generic denial; they
must never reach the agent or reveal agent configuration.

## Errors

- Invalid configuration fails before polling starts.
- Telegram polling errors are logged and handled according to the Telegram
  library's retry behavior.
- A model or fatal agent error produces a short user-facing failure message and
  leaves the current session available for another message or `/new`.
- A Telegram send failure is returned or logged without rerunning the agent
  turn, because rerunning could repeat tool side effects.
- Rate-limit responses honor Telegram's retry delay.

No attempt is made to provide exactly-once processing. Telegram may consider an
update consumed before its agent turn or reply finishes, and a process crash can
therefore lose that turn or reply. Solving this requires a durable inbox and is
outside the personal bridge.

## Testing

Keep the Telegram package testable without contacting Telegram by placing the
small API surface it uses behind an unexported local interface implemented by
the real client and a test fake. This is an implementation seam, not a public
bridge abstraction.

Cover at least:

- Allowed private text reaches the agent and returns its final response.
- Unauthorized users never invoke the agent.
- Group and unsupported messages are ignored.
- Consecutive messages share the current session.
- `/new` and `/clear` replace it with a fresh session.
- Commands are not added to the transcript.
- Turns are serialized.
- Long responses split on valid UTF-8 boundaries.
- Progress is reported during a turn, and the final answer is delivered exactly
  once (on `EventDone`), never duplicated from the terminal `EventAssistant`.
- `stepLine` renders assistant and tool-result events; `ShowThinking` toggles
  only the raw reasoning text, never the presence of a progress step.
- Agent failure sends an error without replaying the turn.
- Context cancellation stops polling and an active run.
- `DiscardStore` satisfies `SessionStore`, saves successfully, and always loads
  as `ErrNotFound`.

## Rollout Steps

1. [x] Add `agent.DiscardStore`, install it by default in `agent.New`, and test it.
2. [x] Add assistant-text extraction and Telegram-safe response splitting helpers.
3. [x] Add the polling-only `bridge/telegram` package with authorization and serial
   text handling.
4. [x] Add `/new`, `/clear`, `/status`, `/help`, and `/start`.
5. [x] Add the `examples/telegram-chat` assembly and setup documentation.
6. Run the bot personally before extracting any general bridge API or adding
   persistence.

## Later, Only When Needed

Real usage may justify:

- `/cancel` with explicit interrupted-turn semantics.
- Persisted sessions and restoration after restart.
- Keeping and selecting old task sessions.
- Durable Telegram update processing.
- Per-chat queues for multiple users.
- A durable named sandbox independent of session resets.
- True token-level streaming via fantasy's `Stream` method, adopted when longer
  or higher-effort outputs approach the non-streaming timeout ceiling, or when a
  transport can consume sub-turn deltas. The `Event` channel already insulates
  the bridge from this change. (Typing actions and edited progress messages are
  no longer deferred — they are mandatory; see Progress.)
- A common bridge abstraction after a second transport exists.

These are follow-on decisions, not requirements for the first useful bridge.
