# Known Bugs

Open findings from two adversarial reviews of the sandbox work
(`SANDBOX-PLAN.md` milestones 0–2): one over the original shell-out Docker
backend plus the registry, one over the moby SDK rewrite that replaced it.

Everything here is **unfixed**. Findings that the SDK rewrite resolved — stderr
dropped on a zero exit, daemon errors indistinguishable from guest exit codes,
unvalidated images, and the unpinned list filter — are gone from this list.

Both reviews were mutation-tested: "no test catches it" below means a mutation
removing the behaviour was actually applied and the suite actually stayed green,
not that nobody looked.

## Verified solid — do not re-audit

Confirmed correct and mutation-resistant, recorded so future readers can skip
them: the registry's generation counter, "never due while an Exec is in flight",
refcount 0 not destroying the entry, `defer box.Close()` not killing a durable
sandbox, durable-without-`Name` validation, non-`Sleeper` arming no timer,
observer delivery genuinely outside the entry lock, Execs genuinely serializing,
spec resolution and write-back ordering, backwards-compatible session decoding,
env discipline, the adopt-or-create decision table, the fake clock, the fake
daemon's stdcopy frame encoding (byte-exact against the real library), and the
`errdefs` not-found unwrapping.

---

## Defects — Docker backend

### DB-1 · `Exec` ignores context cancellation and can hang forever
`agent/sandbox_docker.go:328`

`stdcopy.StdCopy` is a synchronous blocking read on a hijacked `net.Conn`. No
deadline, no goroutine watching `ctx.Done()`. Once hijacked, the connection is
detached from the request context, so the `ctx` passed to `ExecAttach` no longer
governs it.

The model runs `bash -c "tail -f /var/log/x"`, the context is cancelled, and
`Exec` never returns. `agent/registry.go:328` holds `execMu` across `box.Exec`,
so that sandbox is wedged for the life of the process: it never sleeps, and
every later command blocks behind the mutex.

Regression against both `sandbox.Local` — whose doc explicitly promises context
cancellation is an error — and the CLI backend this replaced. The docker CLI
itself selects on `ctx.Done()` here and leaks only a goroutine.

Fix: `att.Conn.SetReadDeadline` driven off `ctx`, or a goroutine that
`att.Close()`s on `ctx.Done()`. No test catches it (demonstrated with a probe).

### DB-2 · Nothing pulls the image
`agent/sandbox_docker.go:194`

`ContainerCreate` does not pull and nothing calls `ImagePull`. The CLI backend
used `docker run`, which does. So `go run . -sandbox docker` on a machine
without `golang:1.26` locally now fails with a wrapped daemon error instead of
fetching it. Silently lost behaviour; neither `examples/code-review/README.md`
(20 lines on image choice) nor `SANDBOX-PLAN.md` mentions the image must already
be present.

### DB-3 · Every `Sleep` costs a full 10 seconds
`agent/sandbox_docker.go:355`

`client.ContainerStopOptions{}` leaves `Timeout` nil, so the daemon uses its 10s
default. PID 1 is `sleep infinity`, which installs no `SIGTERM` handler, and
Linux does not deliver default-disposition signals to PID 1 of a namespace — so
the `SIGTERM` is ignored and the daemon waits the whole grace period before
`SIGKILL`. The grace buys nothing regardless: `docker stop` signals PID 1 only,
never the exec'd children. Hits the registry's idle-sleep path on every sandbox.

Fix: set `Timeout` to 0 (or small) and document why.

### DB-4 · `Close` is unbounded and uncancellable
`agent/sandbox_docker.go:376`

`context.Background()` with no timeout. A wedged daemon wedges `defer
box.Close()` in `agent/run.go:51`. The `Sandbox` interface forces `Close()
error` with no ctx, so `context.WithTimeout` here is the only lever.

### DB-5 · Created-but-unstarted containers leak without a Registry
`agent/sandbox_docker.go:198`

A failed `ContainerStart` deliberately leaves the container for `Reconcile` to
adopt or reap. That holds for `examples/telegram-chat`, which calls `Reconcile`
— but **not** for `examples/code-review`, which wires `DockerFactory` straight
into `WithSandbox` with no Registry. `AutoRemove` does not save it either: it
fires on container *exit*, and a container in state `created` never exits.

Compounding: the mutation "ignore the `ContainerStart` error entirely" survives,
so nothing pins that a failed start is even an error.

The in-code justification is also backwards. `create` holds the container id and
the start error, so it is the only party with certain knowledge that this
container is dead; `Reconcile` is the fallback for a crash, not for a synchronous
error return. A best-effort force remove before returning the start error races
nothing. See DD-5.

### DB-6 · `Mount` without `Dir` silently drops the bind
`agent/sandbox_docker.go:190`

`if f.Mount != "" && f.Dir != ""`. `DockerFactory{Image: x, Mount: abs}` starts
a container with no mount, no error, no event — and the agent then edits files
nobody will ever see. Either error, or default `Dir`.

### DB-7 · A zero `ExecAttachResult` from a `DockerClient` panics
`agent/sandbox_docker.go:322,328`

`DockerClient` is exported, so third parties can implement it. One returning
`client.ExecAttachResult{}, nil` gets a SIGSEGV rather than an error: `att.Reader`
is a nil `*bufio.Reader` inside a non-nil interface, so `Read` panics, and the
deferred `att.Close()` then panics again on the nil `Conn`. Two-line guard.

### DB-8 · Non-running states all map to "asleep"; `Wake` will 409 on some
`agent/sandbox_docker.go:242`

Only `StateRunning` maps to awake, so `paused`, `restarting`, `removing`, `dead`
and `created` all become `SandboxAsleep`. `Wake` (`ContainerStart`) on a paused
container returns 409. `created` is the one that matters, since it is what
Reconcile sees after DB-5.

### DB-9 · `Destroy` is not idempotent and nothing says so
`agent/sandbox_docker.go:211`

A second `Destroy`, or a `Reconcile` racing a manual `docker rm`, returns a
wrapped 404. The integration test papers over it with `_ = f.Destroy(...)`.
`Registry.Reconcile` reaches the backend through this, so decide deliberately.

### DB-10 · `ExecInspect().Running` unchecked — accepted risk, unstated
`agent/sandbox_docker.go:332`

Matching upstream `docker/cli`'s `getExecExitStatus`, which also skips the check,
is defensible and the justification was verified against upstream. But the
residual should be named: if the daemon flushes the stream before recording the
exit code, this returns `ExitCode: 0` for a command that failed, and
`tools/bash.go` then reports success to the model. Same failure *class* as the
D3 this rewrite existed to fix. Either add `if insp.Running` or document the race
in `Exec`'s doc comment. Unpinned in either direction.

---

## Defects — Registry

`agent/registry.go` was out of scope for the SDK rewrite; these are all from the
first review and all still open.

### RG-1 · `Reconcile` reaps every `durable=false` container on the daemon
`agent/registry.go:197`

The listing is daemon-wide and the reap unconditional. Cross-process sharing is a
stated non-goal, but this is not exotic — it is **two of this repo's own
examples**. `examples/code-review -sandbox docker` creates a container labelled
`metaharness.durable=false`; starting `examples/telegram-chat` at that moment
force-removes it mid-review. Also bites a second bridge instance, and any
on-demand `Reconcile` while a non-durable run is in flight.

`TestReconcileReapsThrowawayLeftovers` pins the reap but has no notion of "in
use", so it cannot catch this.

Note what the reap is keyed on: `!info.Durable` over a daemon-wide listing — a
*class*, with no id the caller holds and no user intent behind it. The obvious
fix, an owner label, is not available; see DD-5.

### RG-2 · `Destroy` closes the handle while an `Exec` is running
`agent/registry.go:456` vs `:328`

`entry.exec` takes `execMu` and releases `e.mu` for the duration of `box.Exec`.
`entry.destroy()` takes only `e.mu`, never `execMu`, so `e.box.Close()` runs
while `box.Exec` is still live on the same object — for `dockerBox`, a force
remove racing an exec. Probe-confirmed. Exactly what the entry lock is documented
to prevent.

`-race` does not find it: `SleepySandbox` guards everything with a mutex, so the
permissive fake hides it. Fix: take `execMu` in `destroy`, or defer the `Close`.

### RG-3 · The process-wide lock is held across the backend `Acquire`
`agent/registry.go:103`

`entryFor` holds `r.mu` across `r.inner.Acquire`. Probe-confirmed consequences: a
slow acquire blocks `Snapshot()` **and** acquires of unrelated names.

First message after `METAHARNESS_SANDBOX=work go run .` on a machine without the
image cached: `docker run` pulls, and for those minutes `/status` — the one
command a user would send to find out what is happening — is unanswerable. A
per-name placeholder entry or a per-name `sync.Once` keeps the "inner factory
called once per name" promise without the global stall.

### RG-4 · A panicking inner `Exec` wedges the idler in flight forever
`agent/registry.go:328`

`e.finish()` is called after `box.Exec` but not deferred, so a panic leaves
`inFlight` at 1 and `dueAt()` returning false forever — the sandbox never sleeps
again. Probe-confirmed (`state == "awake"` after advancing four windows). Low
likelihood, one-word fix (`defer e.finish()`), and precisely the leaked-compute
class the plan's leak-safety section exists to prevent.

### RG-5 · First `Acquire` of an untracked stopped sandbox eagerly wakes it
`agent/registry.go:110`

The plan's non-goal is explicit: *"No eager wake on Acquire; wake is lazy on
first Exec."* An untracked name goes to `inner.Acquire`, and `adoptOrCreate`
starts a stopped container. The bridge restarts, the user says "hi", the model
answers with no tool call — the container burns a full idle window for nothing.

The Reconcile path correctly avoids this and is pinned; but Reconcile only knows
containers present at startup and is skipped entirely for a non-`Lister` backend.
`TestRegistryEntryOutlivesItsHandles` asserts "acquiring does not wake" only for
an already-tracked entry, where `entryFor` short-circuits.

The registry already has the machinery — `trackLocked` with a nil box models
"known asleep, attach on first command" — and no way to reach it from `Acquire`:
`adoptOrCreate` starts the container itself (`sandbox_docker.go:144`) and
`entryFor` hard-codes `SandboxAwake` (`registry.go:114`). The missing piece is a
backend that reports the state it adopted instead of normalising it to awake.

### RG-6 · `Reconcile` leaks a freshly acquired handle on a concurrent Acquire
`agent/registry.go:197`

`r.tracks(name)` → `r.inner.Acquire` → `r.track(...)` is three separate lock
acquisitions. If another goroutine tracks the name in between, `trackLocked`
returns the existing entry and the new `box` is dropped without `Close`.
Harmless for `dockerBox`; a real leak for any backend allocating in `Acquire`.

### RG-7 · `Reconcile` reaps a tracked name the backend reports as non-durable
`agent/registry.go:197`

The `!info.Durable` branch is evaluated before `r.tracks(name)`, so the container
is removed while the registry keeps a stale entry pointing at it.

### RG-8 · Reconcile-tracked specs carry no image
`agent/registry.go:197`

The `default` branch tracks a stopped sandbox with `spec.Image` empty. A later
lazy `Acquire` of a container that has since vanished now hits the image
validation added for D9 — a better failure than the old empty-argv path, but
still a spec that has lost the image it should recreate from. Related: the
re-`Acquire` spec's empty image makes `f.image(spec)` fall back to the factory
default, producing a spurious `image_mismatch` warning at every startup when an
adopted orphan's image differs.

Not missing state, though — dropped state. `List` already returns the image
(`sandbox_docker.go:249`) and `Reconcile` discards it: the spec it builds at
`registry.go:211` is `SandboxSpec{Name: info.Name, Durable: true}` and never
reads `info.Image`. Carrying the field through fixes the spurious mismatch in the
same line. Only a container that has vanished entirely leaves nothing to read an
image from, and that case is what `Session.Sandbox` is for.

---

## Test gaps

Correct code, undefended. Each confirmed by a surviving mutation.

| # | Gap | Surviving mutation |
|---|---|---|
| TG-1 | **The exec attach flags and TTY are entirely unpinned.** The fake ignores `AttachStdout`, `AttachStderr` and `TTY`, always emitting both streams multiplexed. `AttachStderr: true → false` survives the whole suite — against a real daemon that is the stderr bug this rewrite existed to fix, verbatim. `TTY: false → true` also survives, and would make `StdCopy` misparse a raw unframed stream. Patching the fake to honour the flags (~8 lines each) turns both into caught mutations. | `AttachStderr: false`, `AttachStdout: false`, `TTY: true` |
| TG-2 | **Daemon-error-vs-exit-code is pinned on one of three exec calls.** Only `ExecCreate`'s failure path is tested. The fake already has a `fail map[string]error` for exactly this and one test uses it. Four four-line tests would close it. | `ExecAttach` err → `ExitCode: 1, nil`; `ExecInspect` err → same; `StdCopy` err swallowed; the `IsNotFound` branch in `execErr` deleted |
| TG-3 | **Read-before-inspect ordering unpinned.** Ordering is correct, but the fake's `ExecInspect` returns the exit code statelessly with no notion of the exec still running. A `drained bool` set by `ExecAttach` would pin it in three lines. | `ExecInspect` moved above `StdCopy` |
| TG-4 | **`WithIdleAfter` is never exercised.** `registry_test.go:21` sets `window = 15 * time.Minute`, which is exactly `DefaultIdleAfter`, so every timing assertion in the file is compatible with the option being ignored. | `WithIdleAfter` → `_ = d` |
| TG-5 | **"Execs on one sandbox serialize" — a stated plan goal — is untested.** Deleting `execMu` passes everything, including the `-race` concurrency test, because `SleepySandbox` is internally mutex-guarded and the test only counts calls. A probe with an overlap-detecting fake shows the real code is correct. | `execMu` removed |
| TG-6 | **"Observer delivered outside the entry lock" is unpinned.** A probe wiring an observer that calls `reg.Snapshot()` shows the real code is correct and does not deadlock; the mutant deadlocks. The plan names this contract explicitly. | `emit` moved inside the lock |
| TG-7 | **`TestRegistryDoesNotSleepDuringACommand:283` is vacuous.** It checks `Pending() == 0` *after* `Advance(4 * window)`, by which point the fake clock has already fired and removed any stale timer. Checking `Pending()` and `DueAt` *before* the advance catches it. | `disarmLocked` removed from `begin` |
| TG-8 | **`TestNewKeepsTheSandbox` passes for the wrong reason.** The test's content comes from `WithSandboxSpec` + Registry, not the `/new` handler. The carry-over in `bot.go` is in fact unreachable in the wired configuration: `newSession()` always returns a zero spec and `SandboxFor` already falls back to the agent default. | the sandbox carry-over deleted |
| TG-9 | **Failed `ContainerStart` behaviour unpinned** — see DB-5. | `ContainerStart` error ignored |
| TG-10 | **`goleak` in the registry tests can never fail.** `registry.go` contains no `go` statement and the fake clock runs callbacks inline, so no goroutine is ever created. Cheap regression insurance, correctly placed — but it currently proves nothing, and the plan lists it as a leak *proof*. | — |
| TG-11 | **No store round-trip for a durable spec.** The legacy→new decode is covered; nothing writes `{Name, Durable}` through `JSONLStore` and reads it back. The wire format was checked by hand and is correct, just unpinned. | — |

Lower-stakes test issues:

- **The fake never splits a read.** `ExecAttach` concatenates every frame into one
  `strings.NewReader` and `StdCopy`'s buffer is 32KB+9, so a single `Read` always
  returns the whole stream — never a partial header, never a straddled frame
  boundary. A hand-rolled demultiplexer would sail through. Latent rather than
  live (the demux is the real library's, and forcing 1- and 2-byte reads keeps
  the suite green), but a `chunk int` on `streamConn` is four lines and would make
  the stderr tests as strong as they read.
- **`switch` used as an assertion chain short-circuits.** `sandbox_docker_test.go:55`
  and `:134` report only the first failure; the use of `t.Errorf` rather than
  `Fatalf` shows all of them were meant to be reported. Should be independent `if`s.
- `fakeExec.workDir` is recorded and never asserted — either pin it or drop it.
- `lastExec()` recovers insertion order by `strconv.Atoi(strings.TrimPrefix(id,
  "exec"))` over map keys, silently skipping unparseable ones. A slice alongside
  the map cannot lie.
- Two dead guards that read as load-bearing: `release()`'s `if e.refs > 0`
  (`registry.go:304`) is unreachable because `sandboxHandle.Close`'s `sync.Once`
  already prevents over-release, and `trackLocked`'s `if state == SandboxAsleep`
  (`registry.go:120`) has no observable effect because a nil box implies no
  sleeper implies no timer. Both removable with the suite green.

---

## Design decisions to make

### DD-1 · The docker backend lives in `agent`, so every consumer compiles moby
`go list -deps ./agent` is **665 packages, 62 of them moby / OCI / containerd /
OpenTelemetry** (the last via `otelhttp` in the SDK's `hijack.go`). Anyone
importing `agent` to use `sandbox.LocalFactory` pays for all of it with no way to
opt out.

This was a deliberate call — keep everything in one package while the shapes are
unsettled — taken before the SDK dependency existed. `SANDBOX-PLAN.md` specifies
`sandbox/docker`, and the move is clean: `sandbox` already imports `agent`, so
nothing new is required.

### DD-2 · Exporting `DockerClient` puts nine moby types in the public API
`client.ContainerCreateOptions`, `ExecAttachResult`, `Filters` and six others are
now part of this module's compatibility surface, so a `moby/moby/client` minor
bump can be a breaking change here — and the module is **v0.5.0, pre-1.0, with a
redesigned API**. The interface exists solely as a test seam. Unexporting it while
keeping the exported `Client` field preserves the seam and the compile-time
`var _` check, and exposes one moby type instead of nine.

### DD-3 · The process-global client caches its construction error forever
`sync.OnceValues` is well reasoned — a client is a connection pool with no
per-factory config, `client.New` performs no I/O, and lazy construction keeps
`import agent` from needing a daemon. Two wrinkles: a `client.New` failure from a
malformed `DOCKER_HOST` or `DOCKER_CERT_PATH` is cached for the process lifetime,
and nothing guards against a future unit test constructing
`DockerFactory{Image: "x"}` without a `Client` and silently reaching the
developer's real daemon — where `OnceValues` means the first such test poisons the
rest of the binary. No test does this today; every non-tagged test injects
`Client`.

### DD-4 · `lint-determinism` misses `time.Since` and `time.Until`
The `FORBIDDEN` alternation lists `Now|After|AfterFunc|Sleep|Tick|NewTicker|NewTimer`.
Both `time.Since` and `time.Until` read the wall clock and both pass — confirmed
by adding each to `registry.go` and watching the target report `ok`. The target
does work otherwise (a raw `time.Now()` is caught) and the `agent/clock.go`
exclusion is appropriately narrow (29 lines, interfaces plus the `systemClock`
adapter). Add `Since|Until`.

### DD-5 · Nothing bounds a container's lifetime, so three defects have to ask "is this mine?"
DB-5, RG-1 and RG-5 are one cause wearing three faces. A container's lifetime is
unbounded by construction, so some *other* actor has to end it — and that actor
cannot tell its own containers from another process's.

`AutoRemove` looks like the bound and is not: it fires on container *exit*, and
PID 1 is `sleep infinity`. Nothing ever exits, so nothing is ever reclaimed. That
pushes reclamation onto `Reconcile`, which holds no id and carries no user intent,
so it reaps by class — every `durable=false` container on the daemon, including
another example's live one (RG-1). The same absence lets `create` leave a failed
start behind for a `Reconcile` that `examples/code-review` never calls (DB-5), and
lets `Acquire` start a container nobody asked to run (RG-5).

The obvious fix is unavailable. **Docker container labels are immutable after
create**, so `metaharness.owner=<id>` can carry an identity but never liveness —
and an owner id without liveness converts RG-1's false reap into a permanent
orphan, because a dead process's id never comes back to claim its container. A
store does not substitute: the effect being recorded lives on another daemon, so
`{write row, call daemon}` has a crash window in either order, `Reconcile` would
then diff three ways instead of two, and `Exec` is not replayable, so an outbox
cannot cover the path the sharpest defects are on. `docker ps` plus labels remains
the better ground truth, per the plan's own reasoning.

Two rules make the question unnecessary rather than answerable:

1. **Lifetime lives with the resource.** Replace the keepalive with a lease the
   container renews or dies on — `while :; do rm -f /run/metaharness.lease; sleep
   <window>; [ -e /run/metaharness.lease ] || exit 0; done`, with `Exec` touching
   the file before running the guest command. Entrypoint exit then means *removed*
   for a throwaway (`AutoRemove` finally has something to fire on) and *stopped*
   for a durable one — which is exactly sleep: disk intact, `ContainerStart`
   adopts it next run. It demands less of the image than bash already does. This
   is SANDBOX-PLAN's deferred dead-man switch promoted from backstop to
   load-bearing: the registry's 15m timer stays the precise fast path, and the
   lease window — comfortably longer, so the registry always acts first in normal
   operation — becomes the floor that survives the process dying. It also retires
   the abandoned-`systemClock`-timers nit below.
2. **Every mutation needs a named requester**: an id the caller is holding, or a
   user asking for a name. Never a class.

The resulting table is the whole decision:

| Actor | May destroy | Justification |
|---|---|---|
| `dockerBox.Close` (throwaway) | its container, by id | created it, holds the id |
| `create`, on failed start | its container, by id | holds the id and the error |
| `Registry.Destroy(name)` | that name | explicit user intent |
| the lease | itself | self-termination |
| `Reconcile` | **nothing** | has neither an id nor intent |

`Reconcile` becomes purely "learn what exists", which makes RG-1 and RG-7
unreachable rather than fixed, and leaves RG-6 as the only defect on that path.

One edge to decide deliberately rather than discover: the lease window doubles as
the maximum duration of a single `Exec`, since nothing renews mid-command. Make it
a `DockerFactory` field and document the contract. Renewing unconditionally on
every `Exec` costs one extra round trip on a local socket and needs no clock; a
renew-only-if-stale variant is available later if it ever shows up in a profile.

---

## Documentation drift

- **`SANDBOX-PLAN.md`** — the CLI→SDK reversal text is accurate and records the
  podman-via-`DOCKER_HOST` caveat and the module cost. But the Structure block
  still places `docker/`, `clock.go`, `idle.go` and `registry.go` under
  `sandbox/` when all four shipped in `agent/`, and the Milestone 0 sketch and
  example-wiring snippet still say `package sandbox/docker` and
  `docker.Factory{...}` where the code and README say `agent.DockerFactory{...}`.
- **`STACK.md`** — updated for the layout change, but its `## Libraries` section
  still lists only `charm.land/fantasy`. The docker SDK is the one hard
  dependency the plan explicitly costed, and it is missing from the document that
  exists to record exactly that.
- **`examples/telegram-chat/README.md:41`** documents the `-model` default as
  `claude-opus-4-8`; `main.go:49` uses `gemma4:31b-cloud`.
- Neither README mentions that the image must already be present locally (DB-2).

---

## Nits

- `examples/code-review/code-review` is an untracked 60 MB arm64 binary that
  `.gitignore` does not cover (it ignores `/bin/`). One `git add -A` from the
  history.
- `/new` replies `sandbox ephemeral (kept)` when there is nothing to keep
  (`bridge/telegram/bot.go:225`).
- Duplicate `adopted` events on the Reconcile path: `DockerFactory.emit` and
  `Registry.emit` both fire, and the telegram example wires `logSandboxEvent`
  into both.
- `idler.execStarted(time.Time)` takes an unnamed, unused parameter. The plan's
  signature has it; the implementation ignores it. Use it or drop it.
- `sleepDue` (`agent/registry.go:398`) checks only the generation, never
  `!now.Before(e.due)`. Correct today because every re-arm bumps the generation,
  but the plan's "ask the idler if still due" implies a deadline comparison that
  is not there — `dueAt()` answers "is there a future deadline", not "has it
  arrived".
- `dockerBox` implements `Sleeper` even for `--rm` throwaway containers, where
  `Sleep` (stop) deletes the container permanently. Unreachable through the
  Registry, since non-durable specs bypass it, but the capability is exported and
  type-assertable.
- `Registry` has no shutdown, so with `systemClock` armed timers are abandoned at
  exit. Fine for a process — but note `systemClock` has 0% coverage; nothing ever
  runs the registry on a real clock.
- `List` reads `container.Summary.Image` while the adopt event reads
  `Config.Image`. The daemon can report the former as `sha256:…` if the tag was
  removed or re-pulled, so the two can disagree in format for the same container.
- `var keepalive = []string{"sleep", "infinity"}` (`agent/sandbox_docker.go:27`)
  is a mutable package-level slice handed straight into `Config.Cmd`; anything
  holding it can mutate every future create.
- `sleep infinity` is a GNU coreutils extension, not POSIX — busybox `sleep`
  wants a number. Latent, because busybox images already fail for want of bash,
  but one more reason for the keepalive to be a shell loop (DD-5).
- `create`'s string return (`agent/sandbox_docker.go:132`) is discarded on the
  adopt path; only the non-durable caller uses it.
- Doc typo at `agent/sandbox_docker.go:31`: "every future method a / method a
  fake has to grow".
- The integration test writes `out.txt` as root inside the container into
  `t.TempDir()`. On Linux with a rootful daemon and a non-root test user, both
  `os.ReadFile` and the `TempDir` cleanup fail — passes on Docker Desktop, fails
  in Linux CI. It also hard-fails rather than skipping when `bash:5` is absent
  locally (DB-2).
