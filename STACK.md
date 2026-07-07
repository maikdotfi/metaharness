## Tech Stack

- Language: Go
- Shape: Go library module assembled by callers

## Libraries

- [charm.land/fantasy](https://pkg.go.dev/charm.land/fantasy) - model,
  message, usage, and schema helper types used by the agent loop and tool
  adapter.

## Layout

```
agent/                    agent loop, sessions, store interface, tool plumbing
model/                    model client abstractions and fantasy adapter
sandbox/                  local sandbox implementation
tools/                    built-in tools
skills/                   bundled skill prompts
examples/                 sample agents and target projects
Makefile                  test target
```

## Getting started

```sh
make test
```

Meta Harness is not executable by itself. Applications import the library,
choose a model, tools, sandbox, and session store, then assemble their own
agent.

Persistence is moving toward an agent database abstraction. See
[`STORE-PLAN.md`](./STORE-PLAN.md) for the intended storage direction.

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
