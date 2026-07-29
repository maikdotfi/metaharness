# What a good public API looks like here

Meta Harness is a library. It has no `main`, so the only place its design is
visible is somebody else's — and the examples under `examples/` are that
somebody. **An example's `main.go` is the API's test suite for taste.** When
assembling an agent takes plumbing, the plumbing is the finding.

This document exists because the sandbox subsystem was built twice. The first
version worked, passed its tests, and was documented at length — and the
documentation was the tell. `docs/sandbox.md` used to open with:

> If you only read one section, read §2 — it answers the "why do we need both of
> these?" question, and the answer is the design in miniature.

A section defending two options that callers kept confusing is a bug report with
a table of contents. `d07dca5` deleted both options. What follows is that diff,
generalised, so the second version is where we start next time.

This is the evidence. The working checklist — the smells, and the checks before
landing — lives in the `public-api` skill
(`.claude/skills/public-api/SKILL.md`), stated once, so an agent changing an
exported surface loads the rules without having to think of opening a doc first.

---

## The bar

Two rules, both checkable by reading a caller and nothing else:

1. **One call per subsystem.** An application says `sandbox.New(kind, opts…)`,
   `agent.New(prompt, opts…)`, `model.New(cfg)`. Not a constructor per layer.
2. **No value held only to surrender.** If the application builds something whose
   sole purpose is to be an argument to the next constructor, that constructor
   should have built it.

Everything below is a way of failing one of those.

---

## The seven moves

### 1. Choose an implementation by name, not by type

The application used to name a concrete backend type, which meant importing the
package that defines it, which meant depending on the Docker SDK to *mention*
Docker:

```go
sandboxes := sandbox.NewManager(sandbox.LocalBackend{Root: root})   // before
```

```go
sandboxes, err := sandbox.New(sandbox.LocalKind, sandbox.WithRoot(root))   // after
```

A backend registers its own kind from `init` (`sandbox/registry.go:50`,
`sandbox/local.go:102`, `sandbox/docker/backend.go:114`), so the *import* is the
switch, and the application says one thing about Docker:

```go
_ "github.com/maikdotfi/metaharness/sandbox/docker"
```

Two properties fall out that are worth wanting on purpose. The set of choices
becomes introspectable — `sandbox.Backends()` is what the example's flag help
prints (`examples/telegram-chat/main.go:62`), so help text follows the imports
instead of being kept in step by hand. And the failure is legible: asking for a
kind nobody registered says *which* kinds exist and that a backend is registered
by importing its package (`sandbox/registry.go:79`).

**Generally:** when a subsystem has interchangeable implementations, register
them by name and let the caller pass a string it can read from a flag. A caller
that must name a type is a caller that must import a dependency.

### 2. Two options that must agree are one option

The worst of the old API:

```go
a := agent.New(systemPrompt,
	agent.WithSandbox(sandboxes),                    // who can mint handles
	agent.WithSandboxSpec(agent.SandboxSpec{Name: name}),  // which sandbox
)
```

Both gone. The sandbox is now a parameter to the session
(`agent/session.go:46`), because there is no such thing as a session without one:

```go
box, err := sandboxes.Open(name)
sess := agent.NewSession(id, modelID, box)
```

**Generally:** two options the caller has to set together, in the right order,
with matching values, are one thing wearing two hats. Options are for what a
caller may *omit*. Anything mandatory belongs in the signature, where the
compiler asks for it.

### 3. Constructors, not struct literals with invariants

```go
sess := &agent.Session{                    // before
	ID:       fmt.Sprintf("review-%d", time.Now().Unix()),
	Model:    modelID,
	Messages: []model.Message{model.NewUserMessage(prompt)},
	Status:   agent.StatusActive,           // ← forget this and the run is wrong
}
```

`Status: StatusActive` was a library invariant delegated to every caller.
`agent.NewSession` sets it. An exported struct is a promise that every zero value
of it is meaningful; when that is not true, export a constructor instead.

### 4. Let the value name itself

The old wiring told the system the sandbox name twice — once in the spec the
agent held, once on the session — and `Agent.Run` reconciled them with a
defaulting rule the caller had to know:

```go
if sess.Sandbox.Name == "" {   // before, in agent/run.go
	sess.Sandbox = a.Sandbox
}
```

Now `Sandbox` has `Name()` (`agent/sandbox.go:28`), so a handle answers for
itself and nothing has to be told which sandbox it is. Where the invariant still
matters — rebinding a restored session — it is *enforced*, not trusted:
`Session.Bind` returns `ErrSandboxMismatch` rather than resuming a task in a
different filesystem (`agent/session.go:66`).

**Generally:** prefer one source of truth that can be asked, over two that must
agree. If a caller could get it wrong, return an error instead of documenting the
right way.

### 5. The common case has no line

`agent.WithStore(agent.DiscardStore{})` disappeared from the Telegram example
because `agent.New` defaults to it (`agent/agent.go:34`). Defaults are how a
library says which case it is for. A required option that almost everyone sets to
the same value is a missing default.

### 6. Ownership is one-directional and stated once

Three owners, three lifetimes, no third party holding anything on anyone's
behalf:

| | owns | releases it by |
| --- | --- | --- |
| application | the `Manager` — goroutines, idle timers, the daemon connection | `Close()` |
| session | one handle, and the name behind it | `sess.Close()` |
| agent | model, tools, prompt, store | nothing |

`Close` on a handle releases a *reference* and never the sandbox, and that is
written on the interface rather than in a doc. The Telegram bridge closes the
sessions it made and nothing else (`bridge/telegram/bot.go`), and it says why in
three lines.

**Generally:** every exported type with a `Close` must answer "and what survives
it?" in its doc comment. Reference-releasing and resource-destroying `Close`
methods that look identical are how callers leak or delete.

### 7. Signatures that admit what can fail

```go
type SessionFactory func() *agent.Session            // before
type SessionFactory func() (*agent.Session, error)   // after
```

Opening a sandbox can fail. The old signature forced the bridge to start a task
with nowhere to work; the new one lets `/new` keep the working session and say
so. An honest signature is cheaper than the recovery code its absence needs.

Also in this category: making a required pass lazy instead of documenting it.
`Reconcile` used to be a call the application had to remember, and forgetting it
leaked compute. Adoption now happens inside `entryFor`; `Reconcile` stays
exported only for the report and the timing.

---

## Leaks still in the tree

The rules above have teeth, which is easiest to show by pointing them at what is
here now. Keep this list current: an entry earns its place by being a caller
doing work the library should do.

- **`examples/code-review/main.go:69`** does
  `filepath.Split(filepath.Clean(workdir))` to turn one path into the
  root-plus-name pair the manager wants. Arithmetic in a caller is a missing
  entry point.
- **Both examples wrap every built-in tool by hand** — `agent.Adapt(tools.Bash{})`
  and three more like it — and both assemble the identical `model.Config` from
  the identical environment variables. Repetition across every caller is the
  library's job, not theirs.
- **`sandbox.WithImage` on a local manager does nothing.** One option vocabulary
  serving every backend is the acknowledged price of choosing a backend by name
  (`sandbox/registry.go:17`), but it is a price, and a caller reading the flag
  help cannot tell which options apply to the kind it chose.

---

## Why this is written down

The first sandbox API was not sloppy. It was carefully built, thoroughly tested,
and had a 1200-line document explaining how to hold it. That document is the
artefact worth remembering: **the effort spent explaining an API is the measure
of how much of it leaked.** `docs/sandbox.md` is now a walkthrough of what
happens, not an apology for how to call it.
