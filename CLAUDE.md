## Rules

- strict TDD: write the failing test first
- tests must test the behaviour, not the implementation
- stay true to idiomatic Go

## Public API

This is a library: its design is only visible in somebody else's `main`. Before
changing an exported surface, write the caller you wish existed — the fragment of
`examples/*/main.go` — and only then the implementation.

- one call per subsystem in a caller; no value built only to be handed to the
  next constructor
- options are for what a caller may omit; anything mandatory goes in the
  signature
- an API change must not make an example longer. If it does, name the trade
  out loud rather than shipping it quietly
- prose that explains how to hold the API is a redesign request, not
  documentation. If the explanation gets longer, the API got worse

**Invoke the `public-api` skill** before changing any exported identifier — a
constructor, option, interface, struct field, method or sentinel error. It holds
the checklist, the smells and the checks before landing. The worked evidence is
[`docs/api-design.md`](./docs/api-design.md): the sandbox redesign (`d07dca5`)
generalised, plus the leaks still in the tree.
