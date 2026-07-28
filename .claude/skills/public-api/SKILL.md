---
name: public-api
description: "Design and review this library's exported surface so it does not leak into consumers. Use when adding or changing any exported Go identifier — a constructor, option, interface, struct field, method or sentinel error — when deciding how an application should wire a subsystem, when an example's main.go or a bridge grows assembly plumbing, when writing or moving a Close method, or when reviewing a diff that touches package-level names. Also use when about to write prose explaining how to call something."
---

# The public API bar

Meta Harness is a library with no `main`. Its design is only visible in somebody
else's — and `examples/*/main.go` is that somebody. **An example's `main.go` is
the API's test suite for taste.** When assembling an agent takes plumbing, the
plumbing is the finding, not the caller's problem.

This skill is the working checklist. The worked evidence — the sandbox subsystem
built twice, with before/after for every rule — is
[`docs/api-design.md`](../../../docs/api-design.md). Read it when you want the
case study or you are about to argue with a rule here.

## Start here: write the caller first

Before the implementation, before the test, type the fragment of `main.go` you
wish existed. Paste it in your response so the human sees it too.

```go
sandboxes, err := sandbox.New(sandbox.LocalKind, sandbox.WithRoot(root))
defer sandboxes.Close()

box, err := sandboxes.Open(name)
sess := agent.NewSession(id, modelID, box)
```

If that fragment is more than **one call per subsystem**, the design is not done
yet. Go back to it before writing any implementation. This is the same discipline
as `CLAUDE.md`'s failing-test-first rule, pointed at the API surface.

## The two rules

1. **One call per subsystem.** `sandbox.New(...)`, `agent.New(...)`,
   `model.New(...)`. Not a constructor per layer.
2. **No value held only to surrender.** If the application builds something whose
   only purpose is to be an argument to the next constructor, that constructor
   should have built it.

## Smells, loudest first

Read the caller, not the library.

| Smell | Why it is a defect |
| --- | --- |
| Prose that reconciles two API elements — "why do we need both of these?" | The strongest signal in this repo's history. It is a redesign request wearing a doc's clothes. |
| A value constructed only to be passed to the next constructor | `NewManager(LocalBackend{Root: root})`: the app assembled the library. |
| The caller translating its vocabulary into ours | Arithmetic in a caller is a missing entry point. |
| The same lines in every example | Repetition across callers is the library's job. |
| A concrete type from an implementation package in an app's imports | Naming a type means importing a dependency. Register by name instead. |
| Our struct literal in an app, with a field that must be set | An exported struct promises its zero value means something. |
| An option whose doc explains when to pass the *other* option | Two options that must agree are one option. |
| A `Close` whose meaning needs a paragraph elsewhere | Reference-releasing and resource-destroying `Close` look identical at the call site. |
| A step the app must remember or a guarantee breaks | Make the pass lazy and internal; export it only for the report or the timing. |

## Decision rules when shaping the surface

- **Mandatory goes in the signature.** Options are for what a caller may omit. If
  forgetting it is a bug, the compiler should ask.
- **Choose implementations by name, not by type.** A registry plus `init`
  registration makes the *import* the switch, and makes the choices
  introspectable (`sandbox.Backends()` feeds the flag help, so help text cannot
  drift from the imports).
- **Let the value name itself.** One source of truth that can be asked beats two
  that must agree — `Sandbox.Name()` removed a defaulting rule from `Run`.
- **Enforce, don't document.** Where the caller could still get it wrong, return
  an error: `Session.Bind` refuses a mismatch with `ErrSandboxMismatch`.
- **The common case has no line.** A required option that everyone sets to the
  same value is a missing default.
- **Every `Close` doc says what survives it.** On the type, not in a doc file.
- **Signatures admit what can fail.** `func() *Session` became
  `func() (*Session, error)` because opening a sandbox can fail, and the bridge
  can then keep the working session instead of starting a task with nowhere to
  work.

## Before landing

1. **Diff the examples.** Every example gets shorter or stays the same length. An
   API change that grows its callers moved work outward — if that is the trade,
   say so out loud in your response rather than shipping it quietly.
2. **Grep what the caller now knows.** No application import of an implementation
   package it did not deliberately choose; no exported struct of ours in an
   application literal with a load-bearing field.
3. **Name what each `Close` does not do**, in the doc comment.
4. **Update the prose in the same commit.** If the explanation got *longer*, the
   API got worse. Doc length is the metric, not the deliverable.
5. **Report the leaks you did not fix** rather than leaving them silent. The
   known ones are listed at the end of `docs/api-design.md`; add to that list.

## Repo rules still apply

Failing test first. Tests exercise behaviour, not implementation. Idiomatic Go.
An API redesign is still a test-first change — the test is usually a caller-shaped
one.
