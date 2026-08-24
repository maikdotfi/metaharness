# MCP Plan

## Direction

An MCP server is another source of tools, and nothing more. Since the
`2026-07-28` stateless model there is no session to keep on our side either: a
server is a value we call, not a resource we hold. Whatever state a server needs
is the server's own business.

So `package mcp` contains every line in the tree that knows the protocol exists,
and it hands the rest of the tree plain `agent.Tool` values. Nothing in `agent`,
`tools` or any example learns a new concept.

There are two doors into it, at different levels:

- **Reflect** — `Server.Tools(ctx)` lists what a server advertises and wraps each
  entry with the server's own schema. One line to wire any server.
- **Declare** — a small `mcp/<vendor>` package hand-writes the tools worth
  exposing, as typed Go args through the existing `agent.Adapt` machinery.
  `Server.Call` is the only primitive it needs.

Reflection is how a server gets used the day it ships. Declaration is how a
server gets used well: curated descriptions, a chosen subset, and a schema the
compiler checks. Both return `[]agent.Tool`, so which door an application uses
changes one line and nothing downstream of it.

## Goals

- One package holding the protocol, the SDK dependency, and nothing else.
- `agent.Tool` out of both doors; no new concept in the agent loop.
- Text and JSON results only.
- No lifecycle for an HTTP server; exactly one owned resource for a stdio one.
- An end-to-end test against a real third-party server (`lightpanda mcp`).

## Non-Goals

- **No stateful MCP features.** Tools only: `tools/list` and `tools/call`. No
  resources, no `subscriptions/listen`, no sampling, elicitation, roots or
  prompts. This — not a protocol version — is what makes our use stateless.
- **No local argument validation on the reflected door.** The server owns its
  schema and can return a curated error; a second opinion here would only reject
  calls the server would have accepted.
- **No images or audio.** Text parts and structured JSON.
- **No per-session connection.** One server value serves every agent session. If
  a server is itself stateful — lightpanda drives one browser — concurrent
  sessions interleave on it, and that is the server's problem by design.
- **No per-turn tool resolution.** The tool set is fixed at `agent.New`;
  `notifications/tools/list_changed` is ignored.
- **No gateway tool** that hides a whole server behind one definition.
- **No MCP server inside a sandbox.** `Sandbox.Exec` collects output and offers
  no pipes, so a stdio server cannot be spoken to through it. Servers run on the
  host, beside the agent process. This is correct for the servers we want.

## Decisions

1. **A server is a value.** `mcp.HTTP` and `mcp.Stdio` do no I/O and cannot fail:
   the first call dials. The failures that matter — unreachable, unauthorized,
   no such tool — are only knowable then, and reach the model as a tool error or
   the wiring code as `Tools(ctx)`'s error.
2. **One type, one `Close`.** `Close` releases what is local — a stdio server's
   subprocess — and never anything on the far side. Closing an HTTP server does
   nothing, and a closed server that is called again simply dials again. So
   `defer srv.Close()` is uniformly correct and never something to think about.
3. **No protocol floor: we support what the SDK supports.** Being the client of
   the official SDK is what makes this free. `Client.Connect` tries the stateless
   `server/discover` first and falls back to the legacy `initialize` handshake by
   itself, and `supportedProtocolVersions` spans `2024-11-05` through
   `2026-07-28`, so no line of our code branches on a version. A floor would only
   refuse working servers: `lightpanda mcp` (`1.0.0-nightly.5445`) answers
   `2024-11-05` whatever version is asked for, and returns `Method not found` for
   `server/discover`. Statelessness is enforced by the Non-Goals above, which hold
   on every version.

   Three things follow from serving both lifecycle models, all small:

   - On a legacy server the session is far-side state, so decision 7's re-dial is
     not free. See there.
   - `StructuredContent` (`2025-06-18`) and `ToolAnnotations` (`2025-03-26`) may
     be absent. The reflected door treats every later field as optional and never
     requires one.
   - `serverInfo` arrives from `initialize` rather than from `server/discover`.
     Same field, same use for naming.
4. **Names are prefixed by the server, and refused rather than mangled.** The
   reflected door exposes `<server>_<wire>`, taking the server's own
   `serverInfo.Name` from the handshake, so nothing has to be told which server it
   is; the declaring door names its tools itself. Invalid runes are replaced to
   meet `^[a-zA-Z0-9_-]{1,128}$`, which providers require and MCP does not — but a
   name that cannot be represented at all, empty or over the limit, is skipped and
   reported through the observer instead of being mangled into something the model
   cannot map back to a tool. So the exposed name is always derivable from the
   rule, and every exception announces itself.
5. **Results collapse to text.** Text parts joined; `StructuredContent`
   JSON-encoded when there is no text; `IsError` passed through as
   `ToolResult.IsError`. Any other content type becomes a one-line placeholder
   naming what was dropped.
6. **Every call has a deadline.** `agent/run.go` sets no per-tool timeout, so a
   hung server would hang a turn forever. Default 60s, `mcp.WithTimeout` to
   override.
7. **A broken connection is not an error state, but it is not silent either.** A
   transport failure clears the cached session, so the next call re-dials and, for
   stdio, respawns. Nothing of ours is lost, because we keep nothing — but a
   legacy server may have been keeping something, and lightpanda's current page is
   exactly that. A call that had to re-dial says so in its result, so the model
   learns the page is gone instead of reading a blank one as fact.

## Prerequisite: `agent.WithTools` becomes additive

`WithTools` is variadic already, but its body replaces the map
(`agent/agent.go:19`), so a second `WithTools` silently discards the first. With
built-in tools and MCP tools arriving from different places that is a trap, and
the alternative — `append(builtins, mcpTools...)` in every caller — is a leak
already named in `docs/api-design.md`. It appends instead, and panics on a
duplicate name the way `sandbox.Register` panics on a duplicate kind: a wiring
mistake, not a runtime condition.

## Public shape

```go
package mcp

// A Server is an MCP server we call. It holds no session state and dials on
// first use.
type Server struct{ /* unexported */ }

func HTTP(endpoint string, opts ...Option) *Server
func Stdio(command string, args []string, opts ...Option) *Server

func (s *Server) Tools(ctx context.Context) ([]agent.Tool, error)
func (s *Server) Call(ctx context.Context, tool string, args any) (agent.ToolResult, error)
func (s *Server) Close() error

func WithBearer(token string) Option
func WithHTTPClient(c *http.Client) Option
func WithTimeout(d time.Duration) Option
func WithObserver(fn func(Event)) Option

// Event reports what a server did. Which fields are set depends on Type.
type Event struct {
	Type     EventType     // dialed, discovered, skipped, called, redialed
	Server   string        // serverInfo.Name once known, else the command or endpoint
	Tool     string        // the tool, on skipped and called
	Protocol string        // the negotiated version, on dialed
	Count    int           // tools exposed, on discovered
	Duration time.Duration // on called
	Err      error
}
```

`Stdio` takes its arguments as a slice rather than variadically so options can
follow, matching `agent.Command`'s `Cmd`/`Args` split. `WithBearer` and
`WithHTTPClient` do nothing to a stdio server — the acknowledged price of one
option vocabulary, as with `sandbox.WithImage` on a local manager.

Reflected, in an application:

```go
browser := mcp.Stdio("lightpanda", []string{"mcp"})
defer browser.Close()

ts, err := browser.Tools(ctx)   // 20 tools, the server's own schemas
if err != nil {
	return err
}

a := agent.New(systemPrompt,
	agent.WithModel(m),
	agent.WithTools(agent.Adapt(tools.Bash{})),
	agent.WithTools(ts...),
)
```

Declared, in an application:

```go
browser := mcp.Stdio("lightpanda", []string{"mcp"})
defer browser.Close()

a := agent.New(systemPrompt,
	agent.WithModel(m),
	agent.WithTools(agent.Adapt(tools.Bash{})),
	agent.WithTools(lightpanda.Tools(browser)...),   // four curated tools, typed
)
```

Declared, in the library — the whole of what a vendor package does:

```go
type GotoArgs struct {
	URL string `json:"url" description:"Absolute URL to load in the browser."`
}

func Goto(s *mcp.Server) agent.Tool {
	return agent.AdaptFunc(
		agent.ToolMeta{Name: "browser_goto", Description: "Load a page in the browser."},
		func(ctx context.Context, ec *agent.ExecCtx, args GotoArgs) (agent.ToolResult, error) {
			return s.Call(ctx, "goto", args)
		},
	)
}
```

## Debugging reflection

Reflection is the one part of this that a consumer cannot read in their own
source: what reached the model came from a server, at runtime. Three things make
that inspectable, in order of how little they cost.

**What already works.** `Tools(ctx)` returns `[]agent.Tool`, and every `agent.Tool`
answers `Definition()` — name, description, and the server's own schema. A
consumer needs no new API to print the catalogue, and we should not invent one for
what a three-line loop already does.

**What is otherwise invisible** is what the observer reports: the negotiated
protocol version, a tool that was skipped and why, and what each call did at the
wire. The last matters most for the declared door, which never lists anything: a
hand-written tool naming a wire tool the server no longer has appears as a
`called` event carrying the error, and nowhere else.

**What a consumer runs when it has gone wrong** is `examples/mcp-inspect`, which
dials a server named on the command line and prints its catalogue: the negotiated
protocol, each exposed name with its required arguments, anything skipped, and the
byte size of each definition plus the total. That last column is the number nobody
measures until their prompt has doubled. It is an example rather than exported
surface, and it doubles as the reflected door's caller.

## Structure

```text
mcp/
  server.go              Server, HTTP, Stdio, options, dial/redial, Close
  server_test.go
  observer.go            Event, EventType, WithObserver
  tools.go               reflected door: Tools(ctx) -> []agent.Tool, name prefixing
  tools_test.go
  call.go                Call, and CallToolResult -> agent.ToolResult
  call_test.go
  lightpanda/
    lightpanda.go        declared door: goto, markdown, links, evaluate
    lightpanda_test.go
    integration_test.go  //go:build lightpanda

agent/
  agent.go               WithTools becomes additive

examples/
  mcp-inspect/
    main.go              dial a server, print what it advertises
```

The SDK is imported from `mcp/` only, aliased `sdk` because our package shares
its name. Nothing outside `mcp/` imports it, and no application ever names it.

## Testing

**Unit, test-first.** The SDK ships a server, so the tests use a real one rather
than a fake: `mcp.NewServer` with `AddTool`, behind
`httptest.NewServer(mcp.NewStreamableHTTPHandler(...))`. That exercises the actual
transport over real HTTP. What each test pins:

- a reflected tool's `Definition()` carries the server's schema verbatim
- a call round-trips arguments and returns the server's text
- a tool that fails server-side becomes `ToolResult{IsError: true}`, not a Go error
- a tool with no arguments is called with `{}` when the model sends `null` or
  nothing, matching `agent/adapt.go`
- names are prefixed with the server's own name and sanitized
- `StructuredContent` with no text parts arrives as JSON
- a call after `Close` succeeds, having dialed again
- a tool whose name cannot be represented is skipped, and the observer says which
  and why, while its siblings are still exposed
- the observer sees the negotiated protocol on dial, and a failed call with its
  error

**Integration, `//go:build lightpanda`.** Real `lightpanda mcp` over stdio,
driven from `make test-lightpanda`:

- the reflected door lists the server's tools, and `goto` is among them
- `goto` then `markdown`, against a page served by `httptest` so the content is
  fixed, through one kept subprocess — which also proves that a stateful server
  works precisely because we keep the connection and it keeps the state
- a declared tool from `mcp/lightpanda` produces the same result as the reflected
  one for the same page
- `Close` reaps the child process
- killing the child mid-session and calling again re-dials, and the result says
  it did

Pinned at `lightpanda 1.0.0-nightly.5445`, which negotiates `2024-11-05`. That
is deliberate: the integration test is the one place we prove the legacy
lifecycle works, since every unit test above runs against a current-protocol SDK
server.

## Rollout Steps

1. `agent.WithTools` additive, with a failing test in `agent` first.
2. `mcp`: `Server`, `HTTP`, `Stdio`, `Close`, `Call`, over the httptest SDK server.
3. The reflected door: `Tools(ctx)`, prefixing, sanitizing.
4. `mcp/lightpanda`: four declared tools.
5. The integration test and `make test-lightpanda`.
6. `examples/mcp-inspect`, which is also how the catalogue gets eyeballed once.
7. Wire it into an existing example, and check the example did not get longer.
8. Docs: package doc, a line in `TOOLS.md` naming this as the case the erased
   `agent.Tool` exists for, the dependency in `STACK.md`.

## Later, deliberately not now

- Per-turn tool resolution, and with it `tools/list_changed`. `Server` would
  satisfy such an interface without changing its own surface.
- A gateway tool, for a server too large to curate and too noisy to reflect.
- Non-text content, which needs `agent.ToolResult` to stop being a string.
- OAuth. `WithHTTPClient` is the escape hatch until something needs more.
- `ToolAnnotations` (`readOnlyHint`, `destructiveHint`) drive an approval policy.
  They are carried through from the start so that stays possible.
