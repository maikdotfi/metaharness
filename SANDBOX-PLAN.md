# Sandbox Plan

> **Building this? Start at "Milestone 0: A Real Sandbox Behind a Flag".** It is
> a self-contained first step that needs none of the design below and changes no
> existing package. The Direction, Goals, and Public Shape sections describe
> where this ends up, not what to write first.

## Direction

Sandboxes are durable, named resources. They are created explicitly, destroyed
explicitly, and merely *referenced* by agents and sessions. Nothing in the agent
loop ever destroys a sandbox: `Close()` on a handle means detach, and an idle
sandbox goes to sleep on its own after a configurable idle window.

The assembling application picks the sandbox by name at startup — typically from
an environment variable. If a sandbox with that name exists, the agent attaches
to it; if not, it is created. Switching sandboxes is therefore a restart with a
different name.

Because `agent.Sandbox` is exec-in/result-out with no long-lived guest
processes, the only state that must survive sleep is the filesystem. Sleep is
therefore defined as **stop compute, keep disk** in every backend. Memory
snapshots (Firecracker) are a faster wake, not a different model.

Sandboxes need no database. Their ground truth lives on the backend — container
name plus labels — and is read back with `docker ps`, not from a store. A table
of sandbox names would be a second source of truth that drifts. The one thing
persistence contributes is the *binding*: `Session.Sandbox` records which
sandbox a session ran in, so a resumed session reattaches to the same one.

The sleep *decision* logic lives in exactly one place — a generic wrapper owned
by a registry — and backends only answer "how do I sleep", never "when".

## Goals

- Reference a sandbox by name: attach if it exists, create if it does not.
- Sleep after N minutes without an `Exec`, implementation-agnostic.
- Wake transparently on the next `Exec`. Callers and tools never know.
- Sandboxes shared across sessions and agents within one process; Execs on one
  sandbox serialize.
- Keep sandbox *selection* in the assembling application. The library takes a
  `SandboxSpec`; it never reads environment variables itself.
- Every component testable deterministically: no `time.Now`, `time.After`,
  `time.Sleep`, or randomness reachable from the sandbox code except through
  injected interfaces. Simulation-style tests advance a fake clock and observe
  exact transitions.
- Backends that cannot sleep (Local, test fakes) keep working unchanged.

## Non-Goals (now)

- No cross-process sharing or locking.
- No memory snapshots; sleep is stop-compute-keep-disk everywhere.
- No eager wake on Acquire; wake is lazy on first Exec.
- No workspace provisioning (repo clones, secrets) in the sandbox layer — see
  the workspace section for where that belongs instead.
- No sandbox state in any store beyond the spec already inside `Session`.
- **No image management and no forking.** This plan consumes `spec.Image` as an
  opaque string and never builds, tags, copies, or forks one. Composing images
  and deriving one sandbox from another are a separate concern for separate code
  above this layer, and nothing here should assume how that works.

## Current State

What exists today, as of the Telegram bridge landing:

- `agent.SandboxSpec` is `{Image string}` — no name, no durability.
- `agent.Sandbox` is `Exec`/`Close`; `agent.SandboxFactory` is `Acquire(ctx,
  spec)`.
- `sandbox.LocalFactory{Root}` is the only backend. It **ignores the spec
  entirely** and hands out `*sandbox.Local{Dir: Root}`, which runs commands on
  the host. `Local.Close` is already non-destructive.
- `testutils` provides `RealSandbox`, `NopSandbox`, and `NopFactory`.
- `Agent` holds `Newbox SandboxFactory` but no spec. `Run` acquires from
  `sess.Sandbox` once per call and `defer box.Close()`.
- `Session.Sandbox SandboxSpec` is persisted by `JSONLStore` (in the meta line)
  and would be persisted by any future snapshot store.

The bridge changes one assumption this plan originally made. A Telegram message
is one `Run` call, so **one turn = one Acquire + one Close**, with minutes of
idle time between turns and `/new` discarding the session but not the sandbox.
That makes detach-on-Close and a refcount that legitimately sits at zero
between turns load-bearing rather than theoretical.

## Milestone 0: A Real Sandbox Behind a Flag

Everything below this section is the destination. This is the first step, and it
is deliberately much smaller: run the `code-review` example in a container
instead of on the host, selected by a flag.

**It needs no changes to the `agent` package at all.** `SandboxSpec.Image`
already exists; `LocalFactory` ignores it and a Docker factory reads it. So no
`Name`, no `Durable`, no `Sleeper`, no registry, no idler, no clock — none of
that is required to prove the swap works.

```go
// package sandbox/docker
type Factory struct {
	Image string // e.g. "golang:1.26"
	Mount string // absolute host path exposed inside the container
	Dir    string       // working directory inside the container, e.g. "/work"
	Client DockerClient // injected; defaults to a process-wide SDK client
}

func (f Factory) Acquire(ctx context.Context, spec agent.SandboxSpec) (agent.Sandbox, error)
```

- `Acquire`: create and start a container from `<image>` running `sleep
  infinity`, bind-mounting `<Mount>` at `<Dir>` with `<Dir>` as the working
  directory and `AutoRemove` set, keeping the container id. The keepalive
  entrypoint is what makes exec possible — a container whose command exits has
  nothing left to exec into. `spec.Image` overrides `Factory.Image` when set.
- `Exec`: exec `<cmd> <args...>` in the container, mapping exit codes exactly
  like `Local` does — a command that ran and exited non-zero is a populated
  `ExecResult` with a nil error, stdout and stderr both filled in; only "never
  ran at all" is an error.
- `Close`: force-remove the container. Ephemeral, matching `LocalFactory`
  semantics today. Detach-on-Close arrives with durability, not here.

The bind mount is the whole substance of "swap out the local folder one":
`LocalFactory{Root: "checkout"}` runs on the host in that directory, so the
container equivalent has to get that directory inside. It must be an absolute
path, which the example resolves with `filepath.Abs`.

### What the image must provide

Not arbitrary. Every tool shells out through `bash -c` (`tools/files.go`), and
the file tools additionally use `cat`, `mkdir`, `dirname`, `printf`, and
`base64`. So:

- The image needs **real bash**. `alpine` fails every tool, because busybox
  provides `ash` and no `bash`.
- The `code-review` prompt asks the agent to run `go test ./...`, so the demo
  image needs a Go toolchain.

`golang:1.26` satisfies both and is the right default for the example. Worth a
`README` note, because "use alpine to keep it small" is the obvious wrong move.

### Example wiring

```go
// examples/code-review/main.go
sandboxKind := flag.String("sandbox", "local", "sandbox backend: local or docker")
image := flag.String("image", "golang:1.26", "container image (-sandbox docker)")

var factory agent.SandboxFactory = sandbox.LocalFactory{Root: workdir}
if *sandboxKind == "docker" {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	factory = docker.Factory{Image: *image, Mount: abs, Dir: "/work"}
}
```

`go run . -sandbox docker` is the demo. The agent reviews the same files, runs
the same tests, and the only difference is where.

### Scope notes

- Testing at this milestone: a fake `DockerClient` covering create/exec/remove,
  plus one build-tagged integration test that runs the real thing. The heavy
  determinism apparatus (fake clock, goleak, `Pending()`) belongs to the
  registry and is not needed yet.
- Containers are ephemeral, so `AutoRemove` plus `Close` is the entire cleanup
  story. No leaks to reconcile, because nothing is meant to survive.
- The agent can write into the mount, exactly as it can write into `checkout/`
  on the host today. Same blast radius as the current example, not a new one; a
  read-only mount option can come later if the review agent should not edit.

## Public Shape

Everything from here on is the durable-sandbox design, to be built after the
demo above works.

### Spec and capability

```go
// package agent
type SandboxSpec struct {
	Name    string `json:"name,omitempty"`
	Image   string `json:"image"`
	Durable bool   `json:"durable,omitempty"`
}
```

Durability is opted into explicitly, never inferred:

- `Durable: true` — the sandbox outlives handles, sessions, and runs; it sleeps
  when idle and only an explicit destroy removes it. Requires `Name` (there is
  nothing to attach to without an identity); the registry rejects a durable
  spec without one at Acquire.
- `Durable: false` (zero value) — today's behavior exactly: created on Acquire,
  gone on Close. `Name` is then purely advisory, passed to the backend as a
  label for humans (readable `docker ps`), with no registry or sharing
  semantics.

Both fields are additive with `omitempty`, so sessions persisted before this
change decode unchanged.

```go
// Sleeper is an optional capability a Sandbox may implement. Sleep releases
// compute but preserves the filesystem; Wake makes Exec possible again.
// Implementations that cannot sleep simply do not implement it.
type Sleeper interface {
	Sleep(ctx context.Context) error
	Wake(ctx context.Context) error
}
```

`Close()` semantics are re-documented: detach the handle. Backends must make it
non-destructive. (`Local.Close` already is.)

### Where the spec comes from

The agent gains a default spec, because the sandbox is now an
application-lifetime choice rather than a per-session one:

```go
// package agent
type Agent struct {
	// ...existing fields...
	Sandbox SandboxSpec // default spec; a session may override
}

func WithSandboxSpec(s SandboxSpec) Option
```

`WithSandbox` keeps taking the factory (the *how*); `WithSandboxSpec` supplies
the identity (the *which*). Resolution in `Run`, before the first Acquire:

1. `sess.Sandbox` if non-zero — a resumed session goes back to the sandbox it
   ran in.
2. otherwise `a.Sandbox`.
3. otherwise the zero spec, which `LocalFactory` ignores — so every existing
   caller and test keeps working untouched.

`Run` then writes the resolved spec back into `sess.Sandbox` before the first
`Store.Save`, so the transcript records which sandbox produced it. Because
adoption is by name, replaying that spec is idempotent: it attaches, it does
not create a second sandbox.

### Naming from the environment

Selection stays in the application, matching how model, tools, and prompt are
already wired:

```go
// examples/telegram-chat/main.go
spec := agent.SandboxSpec{Image: *image}
if name := os.Getenv("METAHARNESS_SANDBOX"); name != "" {
	spec.Name, spec.Durable = name, true
}
a := agent.New(systemPrompt,
	agent.WithSandbox(registry),
	agent.WithSandboxSpec(spec),
	// ...
)
```

Unset variable ⇒ ephemeral, current behavior. Set ⇒ a durable sandbox attached
by name. Changing sandboxes is `METAHARNESS_SANDBOX=other go run .`.

The bridge's `/status` should report the sandbox name and state, and `/new`
should keep the sandbox while discarding the session — the sandbox is the
workbench, the session is the task.

## Structure

Three layers, separated specifically so each is testable in isolation:

```text
agent/
  sandbox.go        Sandbox, Sleeper, SandboxFactory, SandboxSpec (+Name, +Durable)
  agent.go          + Agent.Sandbox, WithSandboxSpec

sandbox/
  local.go          existing host-exec backend (never sleeps; ignores the spec)
  docker/           first real backend: adoption, sleep, destroy
  clock.go          Clock/Timer interfaces (defined at point of use)
  idle.go           pure idle-decision state machine  — layer 1
  registry.go       Registry: refcounts, wraps, sleeps — layer 2

testutils/
  clock.go          fake Clock with Advance()
  sandbox.go        + SleepySandbox fake recording Sleep/Wake/Exec order
```

### Layer 0: the Docker backend (`sandbox/docker`)

This lands first, ahead of the registry, because adoption-by-name is what the
env-variable model needs and because sleep policy is easier to get right with
one real `Sleeper` in front of it than with only a fake.

- Identity is the container name plus labels: `metaharness.sandbox=<name>`,
  `metaharness.durable=true|false`.
- `Acquire` is adopt-or-create: container with this name exists and running ⇒
  attach; exists and stopped ⇒ `docker start` and attach; absent ⇒ `docker
  create`/`run` from `spec.Image`. If an adopted container was built from a
  different image than the spec asks for, attach anyway and report it through
  the observer — the *name* is the identity, per the direction above.
- `Exec` is `ExecCreate` + `ExecAttach` + `ExecInspect`; `Sleep` is
  `ContainerStop`, `Wake` is `ContainerStart`, `Destroy` is a forced
  `ContainerRemove`.
- Ephemeral containers are created with `HostConfig.AutoRemove` so the daemon
  removes them on exit even if `Close` never runs.
- **Reversed:** this plan originally shelled out to the `docker` CLI through an
  injected `func(ctx, args ...string) (stdout string, err error)`. It now talks
  to the daemon through the official SDK (`github.com/moby/moby/client`),
  behind a hand-rolled `DockerClient` interface holding only the calls this
  backend makes. The runner seam could not express two things the `Sandbox`
  contract needs:
  - **stderr on a successful command.** A stdout-only runner has nowhere to put
    the stderr of a command that exits 0, so `git clone`, `npm install` and
    every compiler warning looked silent under Docker and chatty under
    `sandbox.Local`. `ExecAttach` returns one multiplexed stream that
    `stdcopy.StdCopy` splits, on every exit status.
  - **daemon failure vs. guest failure.** With a CLI, `docker exec` exiting 1
    because the container is not running is indistinguishable from the guest
    command exiting 1. The SDK reports the guest's status as
    `ExecInspect().ExitCode`, out of band from the Go error, so the two cannot
    be confused: a stopped or missing container is an `error`, always.

  The cost is ~11 modules and one hard dependency. Podman is still supported,
  but through `DOCKER_HOST` pointing at a podman socket rather than through
  `podman` being argv-compatible with `docker` — the SDK's own connection
  config, which is the only environment reading in this layer.

`Destroy` and a `List` ship with this backend rather than later. Named durable
sandboxes accumulate as soon as you switch names a few times, and without them
the only cleanup path is raw docker commands.

**Accepted interim state:** between the Docker backend landing and the registry
landing, a durable sandbox never sleeps — it stays running until stopped by
hand. For a single-user personal bridge that is one idle container, and the
registry turns it into a pure improvement rather than a fix.

### Layer 1: pure idle state machine (`sandbox/idle.go`)

All sleep policy as a plain struct with no goroutines, no locks, no clock —
callers pass `time.Time` in. This is the part that must be correct, so it is
the part with zero concurrency in it:

```go
// idler decides when a sandbox is due to sleep. Not safe for concurrent use;
// the registry serializes access.
type idler struct { ... }

func (s *idler) execStarted(now time.Time)
func (s *idler) execFinished(now time.Time)
func (s *idler) slept()
func (s *idler) woke(now time.Time)
func (s *idler) dueAt() (time.Time, bool) // next sleep deadline, if any
```

Rules it encodes: never due while an Exec is in flight; due `idleAfter` after
the last Exec finished (or after wake/creation if no Exec yet); never due while
already asleep. Table tests feed literal times and assert `dueAt` — no fake
clock even needed at this layer.

Note the deadline is driven by Exec activity, not by refcount. A chat agent
sits at refcount 0 between every message; sleeping the instant a turn ends
would pay `docker start` on every single turn. The default idle window should
be comfortably longer than a human's typing pause — 15m is a better default for
the bridge than 5m.

### Layer 2: registry (`sandbox/registry.go`)

`Registry` implements `agent.SandboxFactory` by decorating an inner factory. The
application constructs it once at startup and hands it to `WithSandbox`; it is
the process-wide authority, never per-Run.

```go
func NewRegistry(inner agent.SandboxFactory, opts ...RegistryOption) *Registry
// options: WithIdleAfter(d), WithClock(c), WithObserver(f)

func (r *Registry) Acquire(ctx context.Context, spec agent.SandboxSpec) (agent.Sandbox, error)
func (r *Registry) Snapshot() []SandboxInfo
func (r *Registry) Destroy(ctx context.Context, name string) error
```

Behavior:

- `Acquire` with `Durable: true` for the same `spec.Name` returns a handle to
  the **same** entry; the inner factory is called once per name. One entry =
  one mutex, one idler, one timer. A durable spec without a Name is an error.
  Non-durable specs pass straight through to the inner factory (fresh
  ephemeral sandbox, current behavior).
- Each handle's `Close()` decrements a refcount. The entry stays at refcount 0
  — that is precisely when the idle timer matters, and for the bridge it is the
  state between every pair of messages. Only `Destroy` removes entries.
- `Exec` on a handle: lock entry → if asleep, `Wake` → run inner `Exec` →
  update idler → re-arm timer from `dueAt()` → unlock. The lock both
  serializes Execs per sandbox and makes the sleep/wake transition race-free by
  construction.
- Timer callback: lock entry → ask idler if still due (a new Exec may have
  landed; a generation counter discards stale firings) → `Sleep` → unlock.
- If the inner sandbox does not implement `Sleeper`, no timer is ever armed.
  Wrapping `Local` or `NopSandbox` costs nothing.

### Clock DI (`sandbox/clock.go`)

Defined by its consumer, minimal by design:

```go
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}
type Timer interface { Stop() bool }
```

No `Reset` — re-arming is always Stop + new `AfterFunc` with a fresh
generation, which sidesteps the classic `Timer.Reset` race entirely. A
`systemClock` wraps the `time` package. Hand-rolled rather than importing
`clockwork`/`benbjohnson/clock`: the surface is two methods, and owning it keeps
the module dependency-light, which is the whole point of the layout in
STACK.md.

`testutils.Clock` is the fake: `Advance(d)` moves `Now` and runs due callbacks
synchronously on the test goroutine, in deadline order. Synchronous firing is
what makes tests deterministic — after `Advance` returns, every consequence
has happened; nothing to poll or sleep for.

Note: Go ≥1.25 `testing/synctest` is a complementary option (fake time with
real `time` package inside a bubble). Useful later as a second opinion on the
registry's concurrency, but DI remains the primary mechanism because it keeps
the *production* code honest — the compiler proves nothing reaches the system
clock.

## Observability and Leak Safety

Two resource pools can leak, with different lifetimes and different remedies:

1. **In-process**: armed timers, goroutines, refcounts that never return to
   zero. Bounded by process lifetime; proven absent by tests.
2. **On the backend**: compute left running when the process crashes before a
   Sleep fires, or ephemeral sandboxes whose Close never ran. Survives the
   process; caught by reconciliation. Note the durable sandbox's *disk*
   surviving anything is the feature, not a leak — the leakable resource is
   compute.

### State is inspectable, not just logged

The registry exposes its belief as data; callers decide what to log or graph:

```go
type SandboxInfo struct {
	Name     string
	State    State // awake | asleep
	Refs     int
	LastExec time.Time
	DueAt    time.Time // zero when no timer armed
}
```

Snapshot is the leak-audit primitive. Tests assert invariants on it, the
bridge's `/status` reads it, and a future `sandbox ls` CLI reads the same source
of truth.

### Events

Registry option `WithObserver(func(Event))`, called on every transition:
`acquired, released, slept, woke, sleep_failed, wake_failed, adopted,
image_mismatch`. The core stays silent — callers wire slog or metrics counters
themselves. Callbacks are synchronous so tests are deterministic (when a
fake-clock `Advance` returns, every event has been delivered), but delivered
*outside* the entry lock so an observer can never deadlock the registry; the
contract is "fast, non-blocking".

Failure paths are defined and observable rather than silent:

- `Sleep` fails → sandbox stays awake, `sleep_failed` event, deadline re-arms
  and it retries next period.
- `Wake` fails → the `Exec` returns the error, sandbox remains asleep,
  `wake_failed` event.

### In-process leak proofs

- `go.uber.org/goleak` (test-only dependency) at the end of every registry
  test: no goroutine outlives its test.
- The fake Clock exposes `Pending() int`; tests assert zero armed timers once
  all handles are closed and the final sleep has fired.
- Snapshot invariants at teardown: every entry at `Refs == 0`, every
  sleeper-backed entry asleep.
- `SleepySandbox` records Close calls; the ephemeral path asserts exactly one
  Close per Acquire, the durable path asserts zero (detach never closes).

### Backend reconciliation

Ships once both the Docker backend and the registry exist:

- Factory capability `Lister { List(ctx) ([]SandboxInfo, error) }` reads ground
  truth from the backend (`docker ps -a --filter label=metaharness.sandbox`).
- `Registry.Reconcile(ctx)`, run at startup or on demand, diffs belief against
  ground truth: a running durable orphan is adopted as awake with a fresh idle
  deadline (so it sleeps in N minutes like any other); a stopped durable orphan
  is simply known-asleep; an ephemeral orphan is reaped — nothing can reattach
  to it by design. Reconcile returns a report of what it found and did, so the
  caller can log it.
- Dead-man switch (deferred): a guest-side backstop for the crash window —
  container entrypoint exits when a heartbeat file goes stale, where Exec
  touches the file. Bounds leaked compute even if the harness never restarts.
  Same idea as a guest watchdog in VM backends.

## Testing Plan

- `docker/`: a fake `DockerClient` — a tiny in-memory daemon holding container
  state rather than canned answers — so tests say "there is a stopped container
  called work" and assert what the daemon looks like afterwards. The
  adopt-or-create decision table (absent, running, stopped, image mismatch) is
  the part worth exhaustive coverage, plus the `Exec` contract: stderr survives
  a zero exit, the two streams do not interleave, a non-zero guest exit is a
  populated result with a nil error, and a stopped or missing container is an
  error rather than exit 1. One build-tagged integration test against real
  Docker covers the same `Exec` cases end to end.
- `idle.go`: pure table tests. Literal times in, deadlines out. Every policy
  rule is one table row.
- `registry.go` against fakes only (no Docker required, ever, at this layer):
  - `SleepySandbox` in testutils implements `Sandbox + Sleeper` and records an
    ordered log of Exec/Sleep/Wake calls; tests assert exact sequences, e.g.
    `Exec, Exec, (advance idleAfter), Sleep, Exec ⇒ Wake before Exec`.
  - Validation: durable spec without a Name → error from Acquire; non-durable
    spec → passes through untouched, inner factory called every time.
  - Sharing: two durable Acquires of one name → inner factory called once;
    Exec from handle A resets the deadline observed via handle B.
  - Refcounting: Close on both handles → entry survives → advance → Sleep
    fires → re-Acquire reattaches and wakes lazily.
  - Bridge shape: Acquire/Exec/Close repeated N times on one name with short
    advances between → inner factory called once, zero Sleeps; then one long
    advance → exactly one Sleep.
  - In-flight: advance past the deadline *during* a slow Exec (fake sandbox
    blocks on a channel) → no Sleep until it finishes + idleAfter.
  - Non-Sleeper inner sandbox → advance forever → no timer, no calls.
  - `go test -race` with concurrent Execs on one name.
- Spec resolution in `agent`: session spec wins over agent spec; agent spec
  used when the session's is zero; resolved spec written back to
  `sess.Sandbox` and observable in what the store received.
- Agent loop tests: unchanged; `NopFactory` still satisfies the interface. One
  new test wires `Run` through a Registry to prove `defer box.Close()` no
  longer kills anything.
- Discipline: `sandbox` and `agent` packages must not call `time.Now/After/
  Sleep` or `math/rand` directly. Enforced by a grep target in the Makefile
  (cheap forbidigo substitute) so drift fails CI, not review.

## Workspaces (next feature, not this plan)

The intended shape, recorded here so this plan does not paint it into a corner.

A workspace is "a sandbox with a repo checked out and credentials available" —
provisioning that happens **above** the sandbox layer, not inside it. Sandboxes
stay exec-in/result-out; a workspace is a provisioner that runs before an agent
starts using one, plus a couple of small primitives it needs from below.

What the current shape already supports:

- **Working directory** is a sandbox property, not a per-command one —
  `Local.Dir` sets it and `Command` has no `Dir` field. A workspace sets the
  repo root as the sandbox's working directory at create time, and every tool
  inherits it with no change to `Command` or to any tool. Only an agent needing
  to run in a *subdirectory* would force `Command.Dir`; defer until then.
- **Cloning** is just Exec at provision time, so it needs nothing new from this
  layer. Whether a repo instead arrives pre-baked in the image is a question for
  whatever handles images, and a workspace should be written so either works:
  check for the repo, clone only if absent.

What it needs that does not exist yet:

- **Environment for Exec.** Set once at container create (`docker run -e`),
  exposed as something like `SandboxSpec.Env` or a create-time option — *not*
  as a `Command.Env` field. Per-call env would put credentials into every
  Exec's argv and into anything that logs it.
- **Secrets kept out of persistence.** This is the sharp edge, and it follows
  from `SandboxSpec` living inside `Session`: anything in the spec is written
  by the store. So secret *values* must never appear in `SandboxSpec`. The spec
  may name a secret; the application resolves that name to a value and passes
  it to the backend at create time.
- **Secrets kept out of the transcript.** Tool output flows into
  `sess.Messages` and is persisted verbatim, so a single `bash env` or `cat
  ~/.netrc` dumps credentials into the store. Two mitigations, both worth
  planning: prefer narrowly-scoped mechanisms over blanket env (a git
  credential helper, a mounted ssh-agent socket, a short-lived token) so there
  is less to leak, and add a redaction hook on tool results for the rest.

Sketch:

```go
// package workspace
type Spec struct {
	Repo    string // clone URL
	Ref     string // branch, tag, or sha
	Dir     string // path inside the sandbox, e.g. /work/repo
	Secrets []string // names the application resolves; never values
}

func Provision(ctx context.Context, box agent.Sandbox, spec Spec) error
```

Deliberately a plain function over an `agent.Sandbox`, so it composes with
Local, Docker, or any future backend and needs no new interface until a second
provisioner exists to compare against.

## Deferred, with intended structure

- **VM backends**: Firecracker/Cloud Hypervisor on Linux (snapshot/restore as
  a premium `Sleeper` — same interface, faster wake), libkrun or
  Virtualization.framework on macOS (stop-keep-disk sleep, no snapshots).
  Nothing above the `Sleeper` line changes.
- **Cross-process sharing**: today the Registry is the single-process
  authority. If two harness processes must share sandboxes, the Registry's
  narrow API becomes a client to a small supervisor daemon owning the same
  logic; keeping `Acquire`-shaped surface is what makes that swap possible.
- **Policy knobs**: eager wake on Acquire, per-sandbox idle overrides, Exec
  concurrency instead of strict serialization. All live in the registry;
  backends stay policy-free.
- **CLI**: `sandbox ls/rm` over `Snapshot` and `Destroy`. Nothing new to design;
  it is a thin front end on the registry.

## Rollout Steps

**Milestone 0 — a real sandbox, ephemeral, behind a flag.** Ships on its own and
touches no existing package.

1. `sandbox/docker`: `Factory` with an injected `DockerClient`,
   `Acquire`/`Exec`/`Close` as create-and-start / exec / remove. Fake-daemon
   tests plus one build-tagged integration test.
2. `-sandbox docker` and `-image` flags in `examples/code-review`, README note
   on why the image needs bash. **Demo done here.**

**Milestone 1 — durable, named sandboxes.** Everything the env-variable model
needs, still with no sleeping.

3. `agent`: add `SandboxSpec.Name` and `.Durable`, add `Sleeper`, add
   `Agent.Sandbox` + `WithSandboxSpec`, implement spec resolution and
   write-back in `Run`, re-document `Close()` as detach.
4. `sandbox/docker`: labels, adopt-or-create by name, `Sleep`/`Wake` as
   stop/start, `Destroy`, `List`.
5. Wire `METAHARNESS_SANDBOX` into `examples/telegram-chat`; `/status` reports
   the sandbox, `/new` keeps it. Durable sandboxes are usable from here, though
   they stay running until stopped by hand.

**Milestone 2 — sleep.** The part that needs the determinism apparatus.

6. `sandbox/idle.go` + pure table tests.
7. `sandbox/clock.go` + `testutils/clock.go` (fake with synchronous Advance).
8. `sandbox/registry.go` + `testutils.SleepySandbox`: refcounts, sharing,
   sleep, `Snapshot`, `WithObserver`, failure-path semantics, and the
   leak proofs (goleak, `Pending() == 0`, Snapshot invariants). Wire one
   agent-loop test through the Registry.
9. `Lister` + `Registry.Reconcile`, with a startup call in the bridge example.
10. Makefile grep target banning raw `time.Now/After/Sleep` in `agent` and
    `sandbox`.

**Later.**

11. Workspaces, per the section above — starting with sandbox create-time env
    and a redaction hook, since those are the parts this plan owns.
