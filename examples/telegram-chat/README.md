# telegram-chat

A personal Telegram bridge for one assembled Meta Harness agent. It uses
Telegram long polling — no public HTTP listener — and talks to one session at a
time, which `/new` resets. Pass `-db` to keep sessions in a local database and
`/resume` a previous one.

This is deliberately a personal, single-user setup: the agent has file and shell
tools, so it is a privileged interface. Only the numeric Telegram user ids you
list may talk to it; everyone else is ignored.

## 1. Create a bot with BotFather

1. In Telegram, open a chat with [@BotFather](https://t.me/BotFather).
2. Send `/newbot` and follow the prompts to name your bot.
3. BotFather replies with a token like `123456789:AAE...`. This is your
   `TELEGRAM_BOT_TOKEN`. Keep it secret — it is full control of the bot.

## 2. Find your numeric user id

The bridge authorizes by immutable numeric user id, not by username. Message
[@userinfobot](https://t.me/userinfobot) (or any "what is my id" bot) and it
replies with your id, an integer like `11111111`. That is what goes in
`TELEGRAM_ALLOWED_USERS`.

## 3. Run it

```sh
export ANTHROPIC_API_KEY=sk-ant-...
export TELEGRAM_BOT_TOKEN=123456789:AAE...
export TELEGRAM_ALLOWED_USERS=11111111        # comma-separated for more than one

cd examples/telegram-chat
go run . -workdir ./workspace
```

Then open your bot in Telegram and send it a message. `-workdir` is the
directory the agent's file and shell tools operate in; it is created if missing.

Flags:

- `-model` — model id to run (default `claude-opus-4-8`).
- `-workdir` — directory the tools run in (default `workspace`).
- `-show-thinking` — include the model's reasoning text in the progress status
  message. Progress is always shown regardless; this only toggles the reasoning
  text.
- `-think` / `-effort` — enable extended thinking and set its effort level.
- `-sandbox` — where sandboxes live (default `local`). The choices are whichever
  backend packages this binary imports; `-h` lists them.
- `-image` — container image new sandboxes are created from, used only by a
  backend that has images (default `golang:1.26`). It needs real bash, so alpine will not
  do; see the code-review example's README.

## The sandbox

```sh
export METAHARNESS_SANDBOX=work
go run .
```

`METAHARNESS_SANDBOX` names the sandbox the agent works in, and the name is the
whole identity: the bridge creates it on first use and attaches to it every time
after. It is a workbench, not a scratch space — it outlives every message, every
`/new`, and the process itself, and only an explicit destroy removes it. After
five minutes with no command its compute is released and the filesystem stays;
the next message wakes it transparently.

Unset, the name defaults to `default`. Switching sandboxes is a restart with a
different name:

```sh
METAHARNESS_SANDBOX=other go run .
```

The library never reads the environment itself; this example does, and passes the
name in. The first message also reconciles against the backend — the manager does
that itself, so compute a crash left running is bounded by one idle window instead
of running forever, and the bridge has nothing to remember to call.

With `-sandbox local` a sandbox is a directory: `METAHARNESS_SANDBOX=work` runs
commands in `<workdir>/work` on the host, with no isolation and nothing to
release, so releasing compute is a no-op.

With `-sandbox docker` a sandbox is a container of its own, named after it and
created from `-image` on first use. That is the isolation the local backend does
not have, and it makes idle stopping mean something: the container stops after
five idle minutes, keeping its filesystem, and the next message starts it again
with everything where it was. `-workdir` is then unused — the agent works in
`/workspace` inside the container. It needs a reachable Docker daemon
(`DOCKER_HOST` and friends), and `docker ps -a --filter
label=metaharness.sandbox` shows what it has made.

Either way the process logs lifecycle changes as they happen, so an idle stop
shows up in the log rather than only in `/status`.

### Adding or dropping a backend

`-sandbox` takes a name, and the names come from the backend packages `main.go`
imports for their side effect:

```go
_ "github.com/maikdotfi/metaharness/sandbox/docker"
```

That line is this program's entire relationship with Docker. Delete it and
`-sandbox docker` stops being a choice, the Docker SDK leaves the build, and no
other code changes; `local` needs no import because it lives in `sandbox` itself.
The flip side is that a name nothing registered fails at startup rather than at
compile time, so the error says what is available:

```
sandbox: unknown backend "podman": have docker, local (a backend is registered by importing its package)
```

## Commands

- `/new`, `/clear` — discard the current context and start a fresh task, keeping
  the sandbox.
- `/status` — show the session id, the sandbox it is bound to, the model, the
  message count, and token usage.
- `/sessions` — list the stored sessions, most recent first. Only with `-db`.
- `/resume <id>` — continue a stored session where it left off. Only with `-db`.
- `/help`, `/start` — show help.

Resetting starts a new session, so messages, usage, status, and session id are
reset together. It deliberately does not touch the sandbox: the sandbox is the
workbench, the session is the task.

`/sessions` and `/resume` are offered only when the bridge was given a store, and
`/help` lists them only then — a bridge that retains nothing does not advertise
commands it cannot run.

## Persistence

Without `-db` the agent uses `agent.DiscardStore` and the live session is the only
transcript: restarting starts fresh.

With `-db ./sessions.db` every turn is saved to a local Turso database — one row
per session, one row per message — and `/resume` brings a session back. A resumed
session arrives with the *name* of the sandbox it ran in and no live handle; the
bridge opens that name again and binds it, so the task continues in the filesystem
it started in. Binding a different sandbox is refused rather than resumed.

The database is a SQLite-format file, so it can be read by hand:

```sh
sqlite3 sessions.db 'SELECT id, status, sandbox, total_tokens FROM agent_sessions'
sqlite3 sessions.db 'SELECT seq, role FROM agent_messages WHERE session_id = "sess_…" ORDER BY seq'
```

Linking the store is what brings the database driver in. Drop `-db` from your own
assembly and the binary has no driver at all.

## Long polling vs. webhooks

Long polling and webhooks are mutually exclusive. On startup the bridge deletes
any existing webhook for the bot and **drops pending updates**, so messages sent
while it was offline are not replayed as stale prompts. If you previously ran
this bot in webhook mode, that webhook is removed automatically.

## What this is not

There are no groups, media, or webhooks, and no proactive notifications. One chat
talks to one current session: there is no Telegram-chat-to-session mapping, and
`/resume` switches which session that is rather than running two at once. See
[`BRIDGE-PLAN.md`](../../BRIDGE-PLAN.md) for the intended scope and what is
deliberately left for later.
