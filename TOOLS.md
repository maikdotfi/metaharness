# Writing Tools

A tool is something the model can call during an agent run — `bash`, a file
reader, an MCP proxy. Each one takes some arguments the model produces, does
work (usually inside the sandbox), and returns a result the model reads back.

This doc explains how tools are authored, and — more importantly — *why* the
plumbing looks the way it does. The mechanism leans on Go generics and one
helper from the `charm.land/fantasy` library, so both are explained from the
ground up. If you just want to write a tool, skip to
[Writing a new tool](#writing-a-new-tool).

## The problem

The agent loop keeps every tool in one map, keyed by name, and dispatches by
looking the name up (`agent/run.go`):

```go
Tools map[string]Tool
// ...
t := a.Tools[call.ToolName]
res, err := t.Execute(ctx, ec, call.Input)   // call.Input is JSON bytes
```

The model hands us the arguments as **raw JSON bytes**. `bash` wants
`{"cmd": "ls"}`; a hypothetical `read_file` wants `{"path": "/x", "lines": 40}`.
Every tool has a *different* argument shape.

Now here is the constraint. A Go map has one value type. `map[string]Tool`
means every tool is stored as the *same* type, `Tool`. But `bash`'s arguments
and `read_file`'s arguments are different types. There is no way to say "a map
of tools, each with its own argument type" — the moment you put them in one
collection, their individual argument types are **erased**. This is not a Go
wart; any language hits it. A box that holds many different things can only
promise what they have *in common*.

What they have in common is: "I can be handed some JSON and produce a result."
So the shared interface can only speak in raw JSON:

```go
type Tool interface {
	Definition() model.ToolDefinition
	Execute(ctx context.Context, ec *ExecCtx, input json.RawMessage) (ToolResult, error)
}
```

That was the original design, and it worked, but it pushed three chores into
*every* tool:

1. Decode the JSON into a struct by hand.
2. Hand-write a JSON Schema (the description of the arguments we send the model)
   that has to be kept in sync with that struct — two sources of truth that
   silently drift apart.
3. Repeat the "bad input" error handling.

We wanted tool authors to write a normal, typed Go function and never see raw
JSON.

## The idea: generics at the edge, erasure in the map

Go **generics** let you write code parameterised by a type you fill in later.
`TypedTool[T]` reads as "a tool whose argument type is `T`" — `T` is a
placeholder. `TypedTool[BashArgs]` is that same interface with `T` fixed to
`BashArgs`:

```go
// agent/tool.go — what tool authors implement
type TypedTool[T any] interface {
	Meta() ToolMeta
	Execute(ctx context.Context, ec *ExecCtx, args T) (ToolResult, error)
}
```

Notice `Execute` now takes `args T` — a real, typed value. No JSON.

But we established the map can't hold typed tools. So we don't put them in the
map directly. We wrap each one in an **adapter** that satisfies the plain,
erased `Tool` interface, and the map holds the adapters:

```go
// agent/adapt.go
func Adapt[T any](inner TypedTool[T]) Tool { /* ... */ }
```

`Adapt` is the bridge. On one side it accepts a typed tool; on the other side
it hands back a plain `Tool` the map can store. The `T` still exists *inside*
the adapter — it just isn't visible from the map. This is the standard Go move
for "typed leaf, erased at the boundary": the type parameter lives on the
constructor (`Adapt`), not on the interface the collection holds. (You cannot
put a type parameter on an interface a map stores anyway — that is exactly the
erasure problem restated.)

Concretely, the adapter is a small struct that remembers the typed tool and
does the raw-JSON work *once*, in the only place raw JSON now lives:

```go
type typedAdapter[T any] struct {
	inner  TypedTool[T]
	schema schema.Schema
}

func (a *typedAdapter[T]) Execute(ctx context.Context, ec *ExecCtx, input json.RawMessage) (ToolResult, error) {
	// validate, then decode into T, then call the typed tool
	var args T
	// ...
	return a.inner.Execute(ctx, ec, args)
}
```

Every tool author now writes typed code; the JSON boilerplate exists exactly
once, in `agent/adapt.go`.

## Schema from the type: the `fantasy/schema` helper

The model has to be *told* what arguments a tool takes. That description is a
**JSON Schema** — a small document like "an object with a required string field
`cmd`." Previously each tool hand-wrote this as a `map[string]any`. That map and
the Go struct we decoded into were two separate things that had to agree, and
nothing enforced it.

We already depend on `charm.land/fantasy` for the model/message types. It ships
a small, standalone sub-package, `charm.land/fantasy/schema`, that can *generate*
a JSON Schema from a Go type using reflection (inspecting the type's fields at
runtime):

```go
schema.Generate(reflect.TypeFor[T]())   // Go type  -> JSON Schema
schema.ToMap(s)                         // JSON Schema -> map[string]any we send the model
```

It reads struct tags: `json:"cmd"` names the field, a field *without*
`,omitempty` is marked **required**, and a `description:"..."` tag becomes the
field's description shown to the model. So the argument struct becomes the
**single source of truth** — the same type drives both what the model is told
and what we decode into. They cannot drift, because they are the same thing.

Important scoping decision: we use *only* the schema-generation half of that
package. `fantasy` also has a whole agent framework (`AgentTool`, its own run
loop) — we do **not** use any of it; we run our own loop in `agent/run.go`.
`fantasy/schema` is a leaf utility with no ties to that framework, so borrowing
it doesn't couple our tool execution to fantasy's agent machinery. The
`agent` package already imports fantasy; the `tools` package still does not.

## Validation: holding the model to the schema

`fantasy/schema` also *validates* a parsed value against a schema. We opted in
to this, and it's the one piece worth calling out because it changes behaviour.

Plain JSON decoding is forgiving: if the model sends `{}` to `bash`, decoding
into `BashArgs` succeeds and leaves `Cmd` as the empty string `""` — a silent
zero value that we'd then try to run. With validation, a missing required field
(or a bad enum value, or an out-of-range number) becomes a clear error message,
which the adapter returns to the model so it can correct itself on the next
turn:

```go
func (a *typedAdapter[T]) Execute(ctx context.Context, ec *ExecCtx, input json.RawMessage) (ToolResult, error) {
	var obj any
	if err := json.Unmarshal(input, &obj); err != nil {
		return ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if err := schema.ValidateAgainstSchema(obj, a.schema); err != nil {
		return ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	var args T
	if err := json.Unmarshal(input, &args); err != nil {
		return ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return a.inner.Execute(ctx, ec, args)
}
```

We decode twice: once into a generic `any` (which the validator understands —
it works on maps and slices, not on Go structs), and once into the typed `T`
the tool wants. The inputs are tiny, so the double decode is not worth
optimising away.

Note we deliberately did **not** adopt the rest of that package: no JSON
*repair* (fixing malformed/truncated JSON) and no *partial* parsing. Those exist
for streaming responses and for weaker models that emit sloppy JSON. We don't
stream (see the note in `run.go`) and we target frontier models whose tool-call
JSON is already well-formed, so decoding plus validation is enough. If either of
those assumptions changes, the same package has the pieces to drop in.

## Writing a new tool

There are two ways to author one. Both are typed, both derive the schema from
the argument type, and both end up as the same erased `Tool` in the map —
`AdaptFunc` is a thin wrapper over `Adapt`, so there is no runtime or capability
difference. The choice is purely about how the tool's *code* is organised.

**Default to `AdaptFunc`.** Reach for the `TypedTool` interface only when the
tool has grown enough structure that a named type earns its keep — concretely,
when one or more of these is true:

- **It wants helper methods.** Once the logic doesn't fit comfortably in one
  function and you'd split it into pieces, those pieces want to be *methods* on a
  type, sharing its fields — a closure with nested funcs gets unwieldy fast.
- **You want to unit-test its pieces directly.** A named type can be constructed
  with specific config and its methods called in a `_test.go` file, without going
  through tool registration and JSON. A closure can only be exercised as a whole.
- **It has a real configuration surface.** Several fields a caller sets read
  better as a named, documented struct (`ReadFile{Root: ..., MaxBytes: ...}`)
  than as free variables captured by a closure — especially if you construct the
  tool in more than one place.

Note what is *not* on that list: simply *having a dependency* does not force the
interface. A closure captures variables from its surroundings, so a single
injected dependency (a sandbox root, a client) rides along fine in `AdaptFunc`.
The line is "small self-contained function" vs "component with its own
structure," not "stateless vs stateful."

Every tool starts with an **argument struct** — its tags drive the schema:

```go
type ReadFileArgs struct {
	Path  string `json:"path" description:"Absolute path to read."`
	Lines int    `json:"lines,omitempty" description:"Max lines; 0 for all."` // omitempty => optional
}
```

### The short way — `AdaptFunc` (a plain function)

Hand `AdaptFunc` the metadata and a typed handler. No type to declare, no
interface to implement. A captured dependency (`root` here) is fine:

```go
root := "/work"
readFile := agent.AdaptFunc(
	agent.ToolMeta{Name: "read_file", Description: "Read a file from the sandbox."},
	func(ctx context.Context, ec *agent.ExecCtx, args ReadFileArgs) (agent.ToolResult, error) {
		// args.Path is decoded and validated; root is captured from the closure
	},
)
```

This is the equivalent of `charm.land/fantasy`'s function-based tool
constructor, and the right default for most internal tools.

### The full way — implement `TypedTool` (a named type)

Worth it once the tool has helper methods and config you'd rather name and test.
Here the value over a closure is visible: `resolve` is a method sharing the
type's fields, and you can test `ReadFile{Root: "/tmp"}.resolve(...)` on its own:

```go
type ReadFile struct {
	Root     string
	MaxBytes int
}

var _ agent.TypedTool[ReadFileArgs] = ReadFile{} // compile-time check we satisfy it

func (ReadFile) Meta() agent.ToolMeta {
	return agent.ToolMeta{Name: "read_file", Description: "Read a file from the sandbox."}
}

func (r ReadFile) Execute(ctx context.Context, ec *agent.ExecCtx, args ReadFileArgs) (agent.ToolResult, error) {
	path, err := r.resolve(args.Path) // helper method, shares r.Root
	// ...
}

// resolve is a plain method — testable directly, no JSON or registration needed.
func (r ReadFile) resolve(p string) (string, error) { /* jail p under r.Root */ }
```

#### When would `bash` itself earn the full way?

Today's `bash` (see `tools/bash.go`) is a thin pass-through: it runs the string
and returns the output. No fields, no helpers, one method — it ticks *none* of
the boxes above, and is really an `AdaptFunc` at heart. It's written as a type
only because it's the canonical tool we reference by name in tests. So it's a
fair question what would change that. Here is the realistic arc.

Imagine `bash` grows the features a production shell tool eventually needs: a
configurable **timeout**, a **working directory**, **environment variables** to
inject, a **cap on output size** so a runaway `cat` can't flood the context, and
a **deny-list** of commands the agent must never run. Those stop being
incidental — they are the tool's *configuration*. And you'll want more than one
instance: a locked-down `bash` (short timeout, strict deny-list) for an
untrusted agent, and a permissive one elsewhere. Capturing five such settings as
free variables in a closure at each registration site gets muddy; a named,
documented struct you construct with `Bash{Timeout: ..., Deny: ...}` does not.

Each of those settings also brings **logic worth isolating**: checking a command
against the deny-list *before* anything runs, truncating oversized output with a
"…(truncated)" marker, assembling the sandbox command from shell + timeout +
workdir + env. Those want to be methods that share the config fields — and,
crucially, that you can **table-test on their own**: does the deny-list actually
catch `rm -rf /` and its obvious variants? does truncation cut at the right
boundary? You want those tests without booting a shell for each case, which means
calling the methods directly on a constructed `Bash{...}` — exactly what a
closure can't give you.

That is the tipping point. The moment `bash` stops being "run this string" and
becomes "run this string *under a policy I configure and must test*," it has a
configuration surface, helper methods, and unit tests of its own — all three
boxes — and the named type is what holds them together. Until then, `AdaptFunc`
is the honest choice.

### Register it

Both forms produce a `Tool`, so registration is the same. A `TypedTool` must be
wrapped in `Adapt`; an `AdaptFunc` result is already a `Tool`:

```go
agent.New(prompt, agent.WithTools(
	agent.Adapt(tools.ReadFile{Root: "/work"}), // full way -> wrap in Adapt
	readFile,                                    // short way -> already a Tool
	agent.Adapt(tools.Bash{}),
))
```

The one gotcha is the `TypedTool` case: you register `agent.Adapt(tools.Bash{})`,
not a bare `tools.Bash{}`. The compiler enforces it (`WithTools` wants `Tool`,
and a typed tool isn't one until it's adapted), so it's hard to get wrong.

`bash` itself is the worked example — see `tools/bash.go` and its test
`tools/bash_test.go`, which drives a tool through `Adapt` end to end (validation,
decode, real shell execution) and checks that `{}` is rejected before anything
runs.

## Trade-offs

What we gained:

- Tool authors write ordinary typed Go; raw JSON lives in one file.
- The argument struct is the single source of truth for both decoding and the
  schema sent to the model — they can't drift.
- Bad arguments from the model become a clear, self-correcting error instead of
  a silent zero value.

What it costs:

- **A layer of indirection.** To understand a tool's full runtime behaviour you
  read two places: the tool, and `agent/adapt.go`. Someone new has to learn the
  `Adapt` pattern once.
- **Generics in the API.** `TypedTool[T]` / `Adapt[T]` are more to look at than a
  single interface, and the constructor-carries-the-type-parameter shape is
  idiomatic but not obvious to a generics newcomer (hence this doc).
- **The registration gotcha.** Forgetting `Adapt` is a compile error, not a
  runtime surprise — mild, but it is a step you can't skip.
- **A dependency on `fantasy/schema`.** We rely on its reflection rules
  (tag handling, required-field logic). It's a leaf package we don't otherwise
  touch, but it is an external contract our schemas now depend on. A ~40-line
  hand-rolled generator could replace it if that ever became a problem.
- **Double decode per call.** Negligible for tool-sized payloads, noted for
  honesty.

## Considered and rejected

**Leave `Execute` taking `json.RawMessage`** and let each tool decode by hand.
Simpler interface, but it re-introduces the drift between struct and schema and
copies the decode/validate boilerplate into every tool — which is exactly what
this pattern removes.

**Pass the decoded arguments through `context.Context`** (the way HTTP
middleware threads request-scoped data), giving tools a bare
`Execute(ctx, ec)` and having them pull args out with `ctx.Value(key).(T)`.
Tempting by analogy, but it doesn't hold up:

- It saves nothing. The type `T` is still needed to generate the schema and to
  decode the model's JSON — both happen in `Adapt[T]` regardless. Context would
  only change the *last hop* (how the already-decoded value reaches the tool
  body), so you keep all the generics and schema machinery *and* bolt an untyped
  `ctx.Value` + type assertion on top.
- It trades compile-time typing for a runtime assertion that panics (or silently
  zero-values) if the key is missing or wrong — the opposite of what this
  refactor is for.
- It hides the tool's primary input. `Execute(ctx, ec)` no longer says what the
  tool consumes; you'd have to know a magic key exists.
- HTTP handlers only reach for context values because `ServeHTTP(w, r)` is a
  frozen stdlib signature they can't extend. We own `TypedTool`, so that
  constraint doesn't apply. The Go `context` docs say as much: context values
  are for request-scoped data crossing API boundaries, *not* for passing
  parameters to a function — and tool args are exactly parameters.

The clean division we settled on: **primary input → typed parameter (`args T`);
cross-cutting request scope (deadline, cancellation) → `context.Context`;
ambient dependencies (sandbox, session) → `ExecCtx` fields.** Arguments belong in
the first bucket.
