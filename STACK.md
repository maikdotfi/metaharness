## Tech Stack

- Language: Go
- Shape: Go library module assembled by callers

## Libraries

- [charm.land/fantasy](https://pkg.go.dev/charm.land/fantasy) - model,
  message, usage, and schema helper types used by the agent loop and tool
  adapter.
- [github.com/docker/docker/client](https://pkg.go.dev/github.com/docker/docker/client) -
  the Docker daemon SDK, used only by `sandbox/docker`. Nothing else imports it,
  and an application that sticks to the local sandbox never talks to a daemon.
- [turso.tech/database/tursogo](https://pkg.go.dev/turso.tech/database/tursogo)
  - optional embedded, CGO-free session database.

## Layout

```
agent/                    agent loop, sessions, store interface, tool plumbing
agentdb/turso/            embedded Turso session store and schema migrations
bridge/telegram/          personal Telegram long-polling bridge
model/                    model client abstractions and fantasy adapter
sandbox/                  sandbox lifecycle manager and the local backend
sandbox/docker/           Docker backend: one long-lived container per sandbox
tools/                    built-in tools
skills/                   bundled skill prompts
testutils/                reusable fakes and backend behavior suites
examples/                 sample agents and target projects
Makefile                  test targets
```

## Getting started

```sh
make test
```

`make test-docker` additionally runs the sandbox suite against a real Docker
daemon; it needs one running and is otherwise skipped.

Meta Harness is not executable by itself. Applications import the library,
choose a model, tools, sandbox, and session store, then assemble their own
agent.

Persistence is opt-in. `agent.JSONLStore` is the dependency-free filesystem
backend; `agentdb/turso.Store` is the embedded database backend. Both implement
the narrow `agent.SessionStore` and optional `agent.SessionLister` interfaces.

## Logging

Structured logging via `log/slog`, text format on stderr. The level comes from
the `METAHARNESS_LOG_LEVEL` environment variable (`debug` | `info` | `warn` |
`error`); the default `info` is pretty silent — one line when an agent run
starts and finishes.

`debug` shows the raw mechanics of an agent run: the system and user prompts,
every tool call with the exact input the model produced and the exact content
returned (bash, Skill and MCP tools alike), skill loads, raw MCP JSON-RPC
frames, and every step message as serialized JSON.

```sh
METAHARNESS_LOG_LEVEL=debug metaharness agent --agent code-review \
  --workdir ./demo/code-review --prompt "Review this codebase."
```
