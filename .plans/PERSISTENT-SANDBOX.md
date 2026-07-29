# Persistent Sandbox Plan

## Status

Done, all of it, and green under `go test -race` — deliberately in the simplest
form that holds the invariants. What is left is use, not construction.

Done:

- `agent` holds only the contracts: `SandboxSpec{Name, Image}`, `Command`,
  `ExecResult`, `Sandbox`, `SandboxFactory.Open`, plus `WithSandboxSpec` so the
  application chooses the sandbox and the session records it.
- `sandbox.Manager` with `Open`, `Destroy`, `Inspect`, `Reconcile`: lazy prepare
  on first command, per-name serialization, generation-guarded idle stopping with
  retry, transparent wake, terminal destruction, observation-only startup
  recovery.
- `sandbox.Backend`, `sandbox.Clock`, `sandbox.State`.
- `sandbox.LocalBackend`: one directory per sandbox name on the host. It is what
  makes the whole thing runnable before Docker exists, and both examples go
  through a `*sandbox.Manager` over it.
- `sandbox/docker.Backend`: one long-lived container per sandbox name, tested
  against a stateful in-memory daemon and, under `-tags=docker`
  (`make test-docker`), against a real one.
- `sandbox.Event` and `WithObserver`: committed transitions handed to an optional
  callback, so an idle stop is something an application hears about rather than
  infers from a later `Inspect`.
- `examples/telegram-chat`: `-sandbox docker` now builds a real Docker backend,
  and the process logs sandbox events as they commit.

Nothing is outstanding. Two things are worth doing when there is a reason:

1. A per-sandbox named volume, if a sandbox ever has to outlive its own
   container. See the note under "Deliberate simplifications".
2. Exec events, if something ever needs a full audit trail rather than the
   transitions no caller can see. See the same section for why they are absent.

### Deliberate simplifications

The behaviour below is implemented as written. The machinery it prescribed is
not, because in Go it costs more than it pays for here:

- **No reducer, event or effect types.** The transition table is the contract; it
  lives in the `entry` methods (`beginExec`/`endExec`, `beginStop`/`endStop`,
  `beginDestroy`, `observe`) and in the tests. Each `begin*` decides and commits
  the next state under the state lock and returns what the operation needs;
  backend calls happen after it returns, never inside it. Reintroduce a reducer
  only if a state or event arrives that cannot be expressed this way — a queued
  second command, say, or a lifecycle event with no caller.
- **No transient states worth their own machinery.** The operation lock already
  guarantees one outstanding effect per sandbox, so `Preparing`, `Executing`,
  `Stopping` and `Destroying` exist only to make `Inspect` honest about what a
  busy sandbox is doing.
- **Idiomatic names.** `sandbox.State` and `sandbox.Info` rather than
  `SandboxState` and `SandboxInfo`, which stutter at the call site.
- **`ReconcileReport` has no `Reaped`.** Nothing is throwaway any more, so
  recovery has nothing to reap.
- **The fake clock lives in `sandbox/fakes_test.go`, not `testutils`.** A
  `testutils` clock would have to name `sandbox.Timer` to satisfy
  `sandbox.Clock`, and `sandbox`'s own tests could then not import it.
- **No mount in the Docker backend.** The container's own writable layer is the
  sandbox filesystem, which is all the persistence contract needs: a stopped
  container keeps everything and only removal empties it. A host bind mount would
  survive `Destroy`, breaking invariant 6, and a named volume would need creating,
  labelling and removing in step with the container for no gain yet. Only the
  working directory is configured at creation. Add a per-sandbox named volume if
  a sandbox ever has to outlive its own container.
- **A missing exit status is an error, not exit 0.** If the daemon still reports
  a command running after its output has ended, `Exec` fails rather than
  reporting success it cannot vouch for. Reporting zero would tell the agent a
  command it never saw finish had succeeded.
- **No exec events.** The observer reports preparing, stopping, destroying and
  observing — not commands. A caller running a command already holds its result,
  one event per tool call would drown the transitions nobody can otherwise see,
  and every event is a callback the command waits for. Adding them later is
  additive.
- **Observer callbacks keep the operation lock.** They run after the state is
  committed and with no state or map lock held, which is what invariant 12 is
  for, but `opMu` is deliberately still held: it is what makes events for one
  sandbox arrive in the order its transitions happened. Releasing it first would
  let a queued command commit and report before the previous event was delivered.
  The cost is that an observer must not call `Exec` or `Destroy` on the sandbox it
  is being told about, which is documented on `WithObserver`.

## Direction

A sandbox is a persistent, named resource. Its filesystem survives handles,
agent runs, sessions, idle periods, and harness restarts. It is removed only by
an explicit `Destroy`.

Opening a sandbox binds a handle to a name. It does not create compute, start
compute, or wake anything. The first `Exec` makes the named sandbox ready:
the backend finds it, creates it if it does not exist, and starts it if it is
stopped. After the command finishes, the sandbox remains ready until its idle
deadline and then stops while retaining its filesystem.

The lifecycle is defined by one strict state machine per sandbox name. Deciding
the next state is separate from acting on it: a decision is made and committed
under the state lock, and the backend call, timer, or observer callback it
implies happens only afterwards, with no lock held. Its result then commits the
state that follows.

The backend is the ground truth for resources that exist. The in-process
manager owns serialization, idle policy, and its current lifecycle belief.

The sandbox implementation belongs in package `sandbox`. Package `agent` owns
only the small contracts and values the agent loop consumes; it contains no
state machine, lifecycle policy, timer management, or backend implementation.

## Goals

- Address every sandbox by a required, stable name.
- Preserve its filesystem until explicit destruction.
- Make opening and closing handles lifecycle-neutral.
- Create or wake lazily on the first `Exec`.
- Stop compute after a configurable period without an `Exec`.
- Wake transparently on the next `Exec`.
- Serialize commands and lifecycle operations for each sandbox.
- Recover backend state when the harness starts.
- Make every state transition deterministic and exhaustively testable.
- Keep time and backend behavior behind injected interfaces.

## Core Invariants

1. A name identifies at most one sandbox entry in a manager.
2. `Open` never calls the backend.
3. `Close` only closes that handle. It never changes the sandbox lifecycle.
4. Only `Exec` requests readiness from the backend. Reconciliation may observe
   that a sandbox is already ready.
5. Only an accepted current idle deadline stops a sandbox.
6. Only `Destroy` removes its filesystem.
7. At most one command or lifecycle operation runs for a sandbox at a time.
8. No backend call runs while the entry state mutex is held.
9. No backend call runs while the manager map mutex is held.
10. A state change commits before the backend call it decided on begins.
11. A backend result commits the state that follows it.
12. Observer callbacks run after commit, with no state or map lock held. The
    operation lock is deliberately still held; see "Deliberate simplifications".
13. A stale timer firing has no effect.
14. A command is never retried automatically.

## Public Shape

Package `agent` defines only the boundary used by the agent loop:

```go
package agent

type SandboxSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type Sandbox interface {
	Exec(context.Context, Command) (ExecResult, error)
	Close() error
}

type SandboxFactory interface {
	Open(SandboxSpec) (Sandbox, error)
}
```

Package `sandbox` provides the implementation and its management operations:

```go
package sandbox

type Manager struct {
	// unexported state
}

func NewManager(backend Backend, opts ...Option) *Manager

func NewManager(backend Backend, opts ...Option) *Manager // WithClock, WithIdleTimeout

func (m *Manager) Open(agent.SandboxSpec) (agent.Sandbox, error)
func (m *Manager) Destroy(context.Context, string) error
func (m *Manager) Inspect() []Info
func (m *Manager) Reconcile(context.Context) (ReconcileReport, error)

var _ agent.SandboxFactory = (*Manager)(nil)
```

The concrete manager is passed to the agent through the `agent.SandboxFactory`
interface. This keeps the dependency direction one-way: `sandbox` imports
`agent`; `agent` never imports `sandbox`.

`Name` is required. `Image` is creation configuration: it is used when the
backend has no resource with that name. Once the name exists, the existing
resource is authoritative.

`Open` validates the spec, gets or creates the in-process entry, and returns a
lightweight handle. It performs no backend I/O.

`Close` is idempotent. A closed handle rejects later `Exec` calls, but closing it
does not submit a lifecycle event and does not call the backend.

The agent stores the resolved sandbox spec in the session before its first
checkpoint. A resumed session therefore binds the same name.

## Backend Contract

The backend contract is also defined in package `sandbox`, at its point of use:

```go
package sandbox

type Backend interface {
	// EnsureReady finds the name, creates it when absent, and starts it when
	// stopped.
	EnsureReady(context.Context, agent.SandboxSpec) error

	// Exec runs one command in the named, running sandbox.
	Exec(context.Context, string, agent.Command) (agent.ExecResult, error)

	// Stop releases compute and preserves the filesystem.
	Stop(context.Context, string) error

	// Destroy removes compute and filesystem. Missing is success.
	Destroy(context.Context, string) error

	// List reports backend ground truth without changing it.
	List(context.Context) ([]BackendSandbox, error)
}

type BackendSandbox struct {
	Name  string
	Image string
	State BackendState // running or stopped
}
```

`EnsureReady`, `Stop`, and `Destroy` are idempotent with respect to the requested
lifecycle state. `Exec` is not idempotent.

The backend does not decide when a sandbox stops and does not manage idle
timers. It implements requested effects and reports their results.

## Package Layout

```text
agent/
  sandbox.go             SandboxSpec, Command, ExecResult, Sandbox,
                         SandboxFactory

sandbox/
  manager.go             Manager, entry lifecycle, Info, ReconcileReport
  backend.go             Backend, BackendSandbox, BackendState
  state.go               State
  observer.go            Event, EventType, WithObserver
  clock.go               Clock, Timer, system clock adapter
  handle.go              lightweight named handle
  local.go               Local, LocalBackend (a directory per name)
  fakes_test.go          stateful fake backend, deterministic clock
  manager_test.go        orchestration behaviour
  observer_test.go       what a transition reports
  reconcile_test.go      startup recovery behaviour
  local_test.go          local exec and local backend behaviour

  docker/
    backend.go           Docker implementation of sandbox.Backend
    executor.go          command create/attach/copy/inspect
    fakes_test.go        stateful in-memory daemon
    backend_test.go      readiness and lifecycle behaviour
    executor_test.go     command behaviour
    integration_test.go  real-daemon tests, build tag `docker`
```

All lifecycle code is under `sandbox/`. In particular, the entry map, entry
locks, idle timers, reconciliation, and Docker code must not be placed in
`agent/`.

## State Machine

### States

```go
type State uint8

const (
	StateUnknown State = iota
	StatePreparing
	StateReady
	StateExecuting
	StateStopping
	StateStopped
	StateDestroying
	StateDestroyed
)
```

- `Unknown`: the manager has not established current backend state.
- `Preparing`: `EnsureReady` is in progress.
- `Ready`: compute is running and no command is in progress.
- `Executing`: one command is in progress.
- `Stopping`: `Stop` is in progress.
- `Stopped`: compute is stopped and the filesystem remains.
- `Destroying`: `Destroy` is in progress.
- `Destroyed`: the entry is terminal and old handles cannot use it.

The stable states are `Unknown`, `Ready`, `Stopped`, and `Destroyed`. The other
states mean exactly one external effect is outstanding.

### Events and Effects

There are no event or effect types. Each public operation is one function that
holds the operation lock, asks a `begin*` method to decide and commit the next
state, makes at most one backend call with no lock held, and hands the result to
an `end*` or `restore` method to commit the state that follows.

The events in the table below map to entry methods:

| Event | Where it happens |
|---|---|
| `execRequested` | `entry.beginExec` |
| `prepareSucceeded`, `prepareFailed` | `entry.exec` after `EnsureReady` returns |
| `execFinished` | `entry.endExec` |
| `idleDeadline` | `entry.idleReached`, guarded by `entry.beginStop` |
| `stopSucceeded`, `stopFailed` | `entry.endStop` |
| `destroyRequested` | `entry.beginDestroy` |
| `destroySucceeded`, `destroyFailed` | `entry.destroy` after `Destroy` returns |
| `observedRunning`, `observedStopped` | `entry.observe` |

The caller is the goroutine running the operation, so a result is returned
rather than completed through an effect. Nothing is queued: a second command
waits at the operation lock.

### Transition Table

| Current state | Event | Guard | Next state | Effects |
|---|---|---|---|---|
| `Unknown` | `execRequested` | — | `Preparing` | cancel timer, ensure ready |
| `Stopped` | `execRequested` | — | `Preparing` | cancel timer, ensure ready |
| `Ready` | `execRequested` | — | `Executing` | cancel timer, execute |
| `Preparing` | `prepareSucceeded` | — | `Executing` | execute, emit prepared |
| `Preparing` | `prepareFailed` | — | prior stable state | complete command with error, emit prepare failure |
| `Executing` | `execFinished` | — | `Ready` | record completion, arm idle timer, complete command |
| `Ready` | `idleDeadline` | generation is current and deadline is due | `Stopping` | stop |
| `Ready` | `idleDeadline` | generation is stale or early | `Ready` | none |
| `Stopping` | `stopSucceeded` | — | `Stopped` | emit stopped |
| `Stopping` | `stopFailed` | — | `Ready` | arm retry deadline, emit stop failure |
| `Unknown` | `destroyRequested` | — | `Destroying` | cancel timer, destroy |
| `Ready` | `destroyRequested` | — | `Destroying` | cancel timer, destroy |
| `Stopped` | `destroyRequested` | — | `Destroying` | cancel timer, destroy |
| `Destroying` | `destroySucceeded` | — | `Destroyed` | complete destroy, emit destroyed |
| `Destroying` | `destroyFailed` | — | prior stable state | restore timer if needed, complete destroy with error, emit destroy failure |
| `Unknown` | `observedRunning` | during startup reconciliation | `Ready` | arm idle timer, emit observed |
| `Unknown` | `observedStopped` | during startup reconciliation | `Stopped` | emit observed |
| `Destroyed` | `destroyRequested` | — | `Destroyed` | complete destroy successfully |

Unlisted event/state pairs are unreachable rather than checked: holding the
operation lock is what makes a sandbox stable before any operation begins, so
only `Unknown`, `Ready`, `Stopped` and `Destroyed` are ever the current state
when one starts. The two dispositions that are not structural — a destroyed
sandbox refusing work, and a stale or early idle deadline doing nothing — are
guarded explicitly in `beginExec` and `beginStop`.

### Operation Serialization

Each entry has two locks with different purposes:

```go
type entry struct {
	opMu sync.Mutex // one Exec, Stop, or Destroy operation at a time
	mu   sync.Mutex // protects the state below

	state    State
	gen      uint64 // bumped whenever the idle deadline changes
	timer    Timer
	lastExec time.Time
	dueAt    time.Time
}
```

The state lock protects one atomic decision:

```text
lock → check guards → commit next state → unlock → make the backend call
```

The operation lock spans the complete public operation, including its backend
effects. It does not block `Inspect`, because `Inspect` needs only the state
lock. A second `Exec` waits at the operation lock. `Destroy` waits for an active
command to finish. An idle callback waits for an active command and then
rechecks its generation before being accepted.

This gives strict per-sandbox serialization with no command queue to own.

The manager mutex protects only name-to-entry lookup and insertion:

```go
func (m *Manager) Open(spec SandboxSpec) (Sandbox, error) {
	if spec.Name == "" {
		return nil, ErrNameRequired
	}

	m.mu.Lock()
	e := m.entries[spec.Name]
	if e == nil {
		e = newEntry(m, spec)
		m.entries[spec.Name] = e
	}
	m.mu.Unlock()

	return newHandle(e), nil
}
```

No backend operation occurs in this critical section.

## Idle Policy

Idle time begins when an `Exec` finishes. A newly observed running sandbox also
gets a fresh idle window. Opening or closing a handle does not affect the
deadline.

The manager uses injected time:

```go
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) Timer
}

type Timer interface {
	Stop() bool
}
```

Every arm increments the entry generation. The callback carries the generation
it was created with, and `beginStop` accepts the deadline only when:

- the state is `Ready`;
- its generation equals the current generation; and
- the injected clock says the deadline is due.

A failed stop returns the machine to `Ready` and arms one full idle window
before retrying.

A non-positive idle duration disables automatic stopping. It does not change
any other transition.

## Destruction

`Destroy(name)` gets the current entry or creates an `Unknown` entry, takes its
operation lock, and destroys it.

After a successful backend destroy:

1. The entry commits `Destroyed`.
2. Waiting callers receive success.
3. The manager removes the map entry only if the map still points to that exact
   entry.
4. Existing handles continue to point to the terminal entry and reject `Exec`.
5. A later `Open` may create a new entry with the same name.

Backend `Destroy` treats an absent resource as success, making repeated
destruction idempotent.

## Startup Reconciliation

At application startup, `Reconcile` calls `List` once. For each reported name
that is not already known, it creates an entry and records one observation:

- running becomes `Ready`;
- stopped becomes `Stopped`.

A running sandbox receives a fresh idle deadline so compute left running across
a harness restart stops within one configured window.

Reconciliation never starts, stops, or removes a sandbox directly. It only
establishes initial machine state. Subsequent lifecycle changes go through the
normal transition table.

If a sandbox is not observed, its entry remains `Unknown`. Its first `Exec`
resolves absence, creation, and readiness through `EnsureReady`.

## Observability

The manager exposes committed state:

```go
type Info struct {
	Name     string
	State    State
	Image    string
	LastExec time.Time
	DueAt    time.Time
}
```

`Inspect` copies entry state under each state lock and sorts results by name.
It never calls the backend.

`WithObserver(func(Event))` sets an optional callback that receives committed
transitions, so an application can log an idle stop as it happens rather than
noticing it in a later `Inspect`:

```go
type Event struct {
	Type EventType
	Name string
	From State
	To   State
	Err  error
}
```

`From` and `To` are the states either side of the transition, so a failure says
where the sandbox was left as well as that it failed. The types are
`EventPrepared`, `EventPrepareFailed`, `EventStopped`, `EventStopFailed`,
`EventDestroyed`, `EventDestroyFailed` and `EventObserved` — one per reported
transition, so an observer can switch on the type without knowing the table.

Callbacks run synchronously on the goroutine that made the transition, after it
commits. Observers cannot change transition outcomes: there is no error to
return, and nothing is retried on their behalf. A transition that decides against
acting — a stale idle deadline, a destroy of something already destroyed — emits
nothing, because nothing changed.

## Failure Semantics

- `EnsureReady` failure returns the command error and restores the prior stable
  state.
- `Exec` failure still ends the command normally; the sandbox returns to `Ready`
  and receives an idle deadline.
- A command that ran and exited non-zero returns a populated `ExecResult` and a
  nil infrastructure error.
- Context cancellation must interrupt backend stream reads and return promptly.
- `Stop` failure leaves the sandbox `Ready` and schedules a later retry.
- `Destroy` failure restores the prior stable state.
- Observer failure is impossible because observers return no error. A slow
  observer is the caller's own problem: an idle stop waits for it, and a prepare
  is on the path of the command that triggered it.

An interrupted `Exec` is not retried. The caller receives the best result the
backend can establish.

## Docker Backend

Implemented in `sandbox/docker`, over the daemon SDK behind a narrow `daemon`
interface holding only the eleven client calls it uses, which is what the fake
daemon implements.

A sandbox is one container: named `metaharness-sandbox-<name>`, labelled
`metaharness.sandbox=<name>`, and holding the sandbox's filesystem in its own
writable layer. The label is what ownership means — `List` reports a container
only if it carries one, so a container that merely shares the naming convention
is left alone, and the name reported is the sandbox name rather than the
container name so reconciliation can hand it straight back to `Open`. A sandbox
name must be usable as a container name and is refused rather than mangled into
one, since two names must never become one container.

For `EnsureReady`:

1. Inspect the container by sandbox name.
2. If absent, ensure the configured image is available, create the container,
   and start it.
3. If stopped, start it.
4. If running, leave it unchanged.
5. Return success.

An image is inspected first and pulled only when it is absent, and the pull's
progress stream is read to the end, because that is what waits for the image to
land. An image is required to create a sandbox and irrelevant once one exists.

The container has a long-lived keepalive process so commands can be executed
through Docker exec. It replaces the image's own entrypoint, and defaults to
`tail -f /dev/null`: `sleep infinity` is a coreutils extension that the busybox
in Alpine and friends does not understand, and a keepalive that exits takes the
sandbox with it. The configured working directory is established at creation.

Nothing can signal the keepalive — it is PID 1 with no handler — so the
container is created with a stop timeout of zero. Without it every idle stop and
every destroy waits out the daemon's full grace period: measured at 10.3s per
`Destroy` against a real daemon, against 0.26s with it.

Command execution uses create, attach, stream copy, and inspect:

- stdout and stderr are both preserved;
- the stream is fully drained before the exit status is read;
- guest non-zero exit is an `ExecResult`, not a backend error;
- daemon and transport failures are errors;
- context cancellation closes the attached connection under the blocked read,
  which is the only thing that makes an interrupted command return promptly, and
  then waits for the copy to finish before touching its buffers;
- a malformed attach result returns an error rather than panicking;
- an exit status the daemon will not give is an error, not exit 0.

`Stop` asks for an immediate stop and treats not-found as success: a sandbox with
no compute is already in the state `Stop` asks for. `Destroy` force-removes the
named container and treats not-found as success.

`List` filters strictly to containers owned by this sandbox implementation and
reports their names, images, and running/stopped state.

## Testing Strategy

### Manager orchestration

Against a stateful fake backend and fake clock, all written:

- `Open` performs no backend calls, and requires a name;
- the first `Exec` ensures readiness exactly once;
- later `Exec` calls while ready do not prepare again;
- an idle window releases compute and keeps the sandbox;
- the next command wakes it;
- a command replaces the prior idle deadline, and the deadline it replaced cannot
  stop a sandbox when it fires;
- a non-positive idle timeout disables stopping;
- closing any number of handles has no lifecycle effect;
- a closed handle rejects `Exec`;
- one name is one sandbox, however many handles or concurrent `Open` calls;
- commands on one name never overlap;
- commands on different names proceed independently;
- `Inspect` stays responsive during a slow backend call, and reports the command;
- prepare failure restores the prior stable state and the next command retries;
- command failure returns to `Ready` and still starts the idle clock;
- stop failure keeps the sandbox usable and retries after one complete window;
- destroy removes the sandbox, invalidates old handles, and frees the name;
- repeated destroy succeeds, including for a name never seen;
- destroy waits for a command in flight;
- destroy failure leaves the sandbox usable.

### The observer

Against the same fake backend and clock, all written:

- an idle stop is reported, with the transition and the name;
- a failed stop is reported with its error and with `Ready`, where it was left;
- preparing is reported, and a failed prepare with the state it fell back to;
- destruction is reported, and a failed one leaves a usable sandbox;
- reconciliation reports what it found, and only observations;
- commands on a ready sandbox report nothing;
- a deadline a later command replaced reports nothing;
- an observer may call `Inspect` from inside the callback, which is invariant 12
  as a test: it deadlocks rather than fails if a state lock is still held;
- a lifecycle reports its transitions in the order they happened;
- a manager with no observer runs a whole lifecycle unbothered.

Tests advance fake time synchronously and assert exact state immediately. They do
not poll or sleep.

### Reconciliation

Against the stateful fake backend, all written:

- running observations become `Ready` with a deadline;
- stopped observations become `Stopped`;
- unseen names remain `Unknown` and resolve on their first command;
- reconciliation only calls `List`, and changes nothing;
- a running observation stops after one idle window;
- an already-known sandbox keeps its state and its deadline;
- an adopted sandbox is usable without preparing it again;
- a `List` failure is reported.

### Local backend

Written: a name is a directory of its own, the filesystem survives a stop and a
wake, destroy removes it and repeats cleanly, `List` reports what survived, an
unused root is an empty backend, and a name that could escape the root is
rejected rather than resolved.

### Docker

Against an in-memory daemon whose containers exist, run, stop and are removed,
and whose execs produce real multiplexed streams over real connections, all
written:

- absent, running and stopped containers;
- an image is pulled only when absent, and creation needs one only when there is
  no sandbox yet;
- waking a stopped sandbox starts it and never recreates it;
- stdout and stderr on both zero and non-zero guest exits;
- output split across interleaved frames, and output past one read;
- a daemon error written into the stream;
- cancellation of a command still producing output;
- a stopped or missing container during command execution;
- a malformed inspect response and a malformed attach response;
- an exit status the daemon will not give;
- an immediate stop, and stopping something that is not there;
- idempotent destruction;
- the list ownership filter, including a container named like ours but unlabelled;
- an error from every daemon call in every path.

Two of the fake daemon's behaviours are deliberately harsh, because a lenient
fake would hide the bug: creating a container needs its image present, and an
image only becomes present once the pull's progress stream has been read to the
end; and an exec reports itself running until its output has ended, so reading
the exit status too early yields no status at all.

The `docker`-tagged suite (`make test-docker`) exercises the same lifecycle and
command contract against a real daemon, and skips when there is none. It also
covers the two things only a real daemon shows: that stopping and destroying do
not wait out a grace period, and that cancellation returns promptly.

### Discipline

- Production sandbox code reads time only through `Clock`.
- Tests do not use wall-clock sleeps.
- Run the manager suite under `go test -race`.
- Fakes model backend state rather than returning scripted answers.
- Write each behavioral test before its implementation.

## Implementation Sequence

### 1. Contract — done

1. Keep only `SandboxSpec`, command/result values, and the sandbox interfaces in
   `agent`.
2. Define `State`, `Backend` and `Clock` in `sandbox`.

### 2. Manager — done

3. `sandbox.Manager` with name validation, the entry map, and backend-free
   `Open`.
4. Per-entry state and operation locking.
5. Handle closure, command serialization, and destruction.
6. Injected timers, generations, idle stopping, and retry behaviour.
7. Inspection.

### 3. Backend — done

8. `sandbox.LocalBackend`, a directory per name.
9. `sandbox/docker.Backend` against a stateful fake daemon.
10. Cancellation-safe command streaming and exit inspection.
11. The real-daemon integration suite.

### 4. Recovery and agent wiring — done

12. Observation-only startup reconciliation in `sandbox`.
13. Resolve the application sandbox spec and persist it in the session.
14. Wire the examples through `*sandbox.Manager`.

### 5. Observation — done

15. The transition observer, and the telegram-chat example logging it.

## Completion Criteria

Met:

- all lifecycle implementation lives in package `sandbox`;
- package `agent` contains only the sandbox contracts used by the agent loop;
- every sandbox requires a name;
- opening and closing handles produce no backend lifecycle calls;
- the first command creates or wakes transparently;
- commands serialize per name;
- idle compute stops while filesystem state survives;
- the next command wakes and sees the same filesystem;
- explicit destruction invalidates old handles and removes the resource;
- restart reconciliation bounds running compute to one idle window;
- every reachable state/event combination has a tested disposition;
- no state or manager lock is held across backend I/O;
- all timing tests are deterministic;
- the race detector passes;
- a real backend that has compute to release, and its real-daemon lifecycle
  suite;
- committed transitions are observable as they happen.

Nothing outstanding.
