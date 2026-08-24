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
- [github.com/modelcontextprotocol/go-sdk](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp) -
  the official MCP client, used only by `mcp/`. Being its client is what lets us
  branch on no protocol version: it negotiates `2024-11-05` through `2026-07-28`
  by itself. Nothing outside `mcp/` imports it, and no application names it.
- [turso.tech/database/tursogo](https://pkg.go.dev/turso.tech/database/tursogo)
  - optional embedded, CGO-free session database. It speaks the SQLite query
  language and file format and reaches its Rust core through `purego`, so a
  session database is readable with the `sqlite3` CLI and needs no toolchain.
  Only `agentdb/turso` imports it.

## Layout

```
agent/                    agent loop, sessions, store interface, tool plumbing
agentdb/turso/            embedded Turso agent database: sessions as rows, one
                          row per message, the agent's notes, and the schema
                          migrations
bridge/telegram/          personal Telegram long-polling bridge
model/                    model client abstractions and fantasy adapter
sandbox/                  sandbox lifecycle manager and the local backend
sandbox/docker/           Docker backend: one long-lived container per sandbox
mcp/                      MCP servers as sources of agent.Tool: reflect a whole
                          server, or declare a curated subset
mcp/lightpanda/           declared browser tools over `lightpanda mcp`
memory/                   what an agent knows between sessions, rendered into
                          its system prompt
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
daemon; it needs one running and is otherwise skipped. `make test-lightpanda`
runs the MCP suite against a real `lightpanda mcp` subprocess, which is where the
legacy protocol lifecycle is proven.

Meta Harness is not executable by itself. Applications import the library,
choose a model, tools, sandbox, and session store, then assemble their own
agent.

Persistence is opt-in, and package `agent` holds no implementation of it beyond
`DiscardStore`, the default. An application that wants sessions to survive a
restart passes `agentdb/turso.Store` to `agent.WithStore`; one that does not
links no database driver at all. A store implements the narrow
`agent.SessionStore` and, to be listable, the optional `agent.SessionLister`.

Memory is that choice made again for what outlives a session:
`agent.WithMemory(memory.SystemPrompt(store))` renders the agent's notes into its
system prompt and gives the model one tool to write them.

## Logging

Structured logging via `log/slog`, text format on stderr. The level comes from
the `METAHARNESS_LOG_LEVEL` environment variable (`debug` | `info` | `warn` |
`error`); the default `info` is pretty silent — one line when an agent run
starts and finishes.

`debug` shows the raw mechanics of an agent run: the system and user prompts,
every tool call with the exact input the model produced and the exact content
returned, skill loads, and every step message as serialized JSON. What an MCP
server did is not in there — `mcp.WithObserver` reports that to the application,
which decides whether it is a log line.

```sh
METAHARNESS_LOG_LEVEL=debug metaharness agent --agent code-review \
  --workdir ./demo/code-review --prompt "Review this codebase."
```
