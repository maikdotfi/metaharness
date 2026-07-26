# The sandbox subsystem

How `sandbox/` and `sandbox/docker/` are put together, for someone reviewing the
code for maintainability. Every claim here is traceable to a file and line; the
last section collects things a reviewer should look at deliberately.

## 1. What problem it solves

An agent needs somewhere to run shell commands. That place must:

- **outlive one agent run** — the same name is the same filesystem tomorrow,
  in another session, after a restart (`agent/sandbox.go:5-9`);
- **not cost compute while nobody is using it** — commands arrive in bursts
  separated by model latency and by hours of nothing;
- **wake transparently** — the caller runs a command; whether that meant
  creating a container, starting a stopped one, or nothing at all is not their
  problem.

The design answer is a split: **the backend owns existence, the manager owns
lifecycle policy, the handle owns nothing.**

## 2. Layers

```mermaid
flowchart TB
    subgraph app["application / agent loop"]
        loop["agent.Agent.Run<br/><i>agent/run.go:45</i>"]
        tools["tools (bash, files, …)"]
    end

    subgraph iface["agent package — interfaces only"]
        factory["SandboxFactory<br/>Open(spec) → Sandbox"]
        sb["Sandbox<br/>Exec, Close"]
    end

    subgraph mgr["sandbox package — policy"]
        handle["handle<br/><i>handle.go</i>"]
        manager["Manager<br/><i>manager.go</i>"]
        entry["entry (one per name)<br/><i>manager.go:186</i>"]
        clock["Clock / Timer<br/><i>clock.go</i>"]
        obs["Event / observer<br/><i>observer.go</i>"]
    end

    subgraph back["Backend — existence"]
        local["LocalBackend<br/>a directory per name<br/><i>local.go:69</i>"]
        dock["docker.Backend<br/>a container per name<br/><i>docker/backend.go</i>"]
    end

    loop --> factory
    tools --> sb
    factory -.implemented by.-> manager
    sb -.implemented by.-> handle
    handle --> entry
    manager --> entry
    entry --> clock
    entry --> obs
    entry --> local
    entry --> dock
    dock --> daemon[("Docker daemon<br/>(Podman-compatible)")]
```

Two properties fall out of this shape and are worth checking hold everywhere:

1. `agent` depends on **no** sandbox implementation — only on `Sandbox` and
   `SandboxFactory` (`agent/sandbox.go:31,39`). The Docker SDK is imported by
   `sandbox/docker` and nothing else.
2. `Backend` **decides nothing**. Idle policy, serialization and timers live
   above it (`backend.go:9-14`). A new backend is five methods, no policy.

## 3. Types and contracts

```mermaid
classDiagram
    class Sandbox {
        <<interface>>
        Exec(ctx, Command) ExecResult, error
        Close() error
    }
    class SandboxFactory {
        <<interface>>
        Open(SandboxSpec) Sandbox, error
    }
    class Backend {
        <<interface>>
        EnsureReady(ctx, spec) error
        Exec(ctx, name, cmd) ExecResult, error
        Stop(ctx, name) error
        Destroy(ctx, name) error
        List(ctx) []BackendSandbox, error
    }
    class Manager {
        -backend Backend
        -clock Clock
        -idle Duration
        -observer func(Event)
        -mu Mutex
        -entries map~string, entry~
        +Open(spec) Sandbox, error
        +Destroy(ctx, name) error
        +Inspect() []Info
        +Reconcile(ctx) ReconcileReport, error
    }
    class entry {
        -spec SandboxSpec
        -opMu Mutex
        -mu Mutex
        -state State
        -gen uint64
        -timer Timer
        -lastExec, dueAt Time
    }
    class handle {
        -entry *entry
        -closed atomic.Bool
    }

    SandboxFactory <|.. Manager
    Sandbox <|.. handle
    Sandbox <|.. Local
    Backend <|.. LocalBackend
    Backend <|.. DockerBackend
    Manager "1" --> "*" entry
    handle --> entry
    Manager --> Backend
```

The contract that makes the whole thing predictable is the **error/exit-code
split**, repeated identically in `Local.Exec`, `LocalBackend.Exec` and
`docker.Backend.Exec`:

| Outcome | Return |
| --- | --- |
| command ran, exited 0 | `ExecResult{ExitCode: 0}`, `nil` |
| command ran, exited non-zero | `ExecResult{ExitCode: n}`, `nil` |
| command never ran, or its outcome can't be established | zero `ExecResult`, non-nil error |

The agent loop treats a non-nil error as fatal infrastructure failure, so the
line matters: a failing `grep` must never look like a broken sandbox.

Idempotency is the other half of the `Backend` contract (`backend.go:15-16`):
`EnsureReady`, `Stop` and `Destroy` are idempotent *with respect to the state
they ask for*; `Exec` is not. "Already gone" is success for `Stop` and `Destroy`.

## 4. Manager state machine

`State` (`state.go`) has four resting states and four that mean "one backend call
is in flight".

```mermaid
stateDiagram-v2
    [*] --> Unknown : Open (no I/O)

    Unknown --> Preparing : first Exec
    Stopped --> Preparing : Exec wakes it
    Preparing --> Executing : EnsureReady ok
    Preparing --> Unknown : EnsureReady failed
    Preparing --> Stopped : EnsureReady failed<br/>(restore prior)

    Ready --> Executing : Exec (already ready)
    Executing --> Ready : command finished<br/>(ok or non-zero)

    Ready --> Stopping : idle deadline reached
    Stopping --> Stopped : backend Stop ok
    Stopping --> Ready : Stop failed<br/>(re-arm one window)

    Unknown --> Destroying : Destroy
    Ready --> Destroying : Destroy
    Stopped --> Destroying : Destroy
    Destroying --> Destroyed : ok
    Destroying --> Ready : failed (restore)
    Destroyed --> [*] : forgotten by Manager

    note right of Ready
        On the idle clock.
        dueAt set, timer armed.
    end note
    note right of Destroyed
        Handles get ErrDestroyed.
        The name is free to reuse.
    end note
```

Resting vs. in-flight is not decoration — `Inspect` reads committed state only
(`manager.go:94`), so `preparing` / `executing` are what an operator sees while a
backend call is outstanding.

## 5. Concurrency: two locks, three rules

`entry` holds two mutexes with sharply different scopes (`manager.go:186-202`).

```mermaid
flowchart LR
    subgraph e1["entry &quot;alpha&quot;"]
        op1["opMu — held across backend calls<br/>serializes exec / stop / destroy"]
        st1["mu — guards state, gen, timer<br/>never held across a backend call"]
    end
    subgraph e2["entry &quot;beta&quot;"]
        op2["opMu"]
        st2["mu"]
    end
    mmu["Manager.mu — guards the name map only"]

    A["Exec on alpha"] --> op1
    B["Exec on beta"] --> op2
    C["Inspect()"] --> mmu
    C -.reads.-> st1
    C -.reads.-> st2
    D["Open / Destroy"] --> mmu
```

The three rules to hold the line on in review:

1. **`Manager.mu` guards the lookup and nothing else** (`manager.go:35`) — a slow
   backend call on one sandbox never blocks opening or inspecting another.
   Covered by `TestSandboxesRunIndependently`, `TestInspectStaysResponsiveDuringCommand`.
2. **`opMu` is held across backend calls** — this is the *only* thing serializing
   work per sandbox, and also what orders events. Covered by
   `TestCommandsSerializePerSandbox`, `TestDestroyWaitsForRunningCommand`.
3. **`mu` is never held across a backend call** — hence the `begin*` / `end*`
   pairs (`beginExec`/`endExec`, `beginStop`/`endStop`, `beginDestroy`). Any new
   operation must follow the same shape.

## 6. The idle policy

A sandbox goes on the idle clock whenever it comes to rest in `Ready`. The
subtlety is the **generation counter**: a fired timer must not stop a sandbox
that was used while the timer was in flight.

```mermaid
sequenceDiagram
    autonumber
    participant C as caller
    participant E as entry
    participant K as Clock
    participant B as Backend

    C->>E: Exec (#1)
    E->>E: beginExec — cancelIdle, state=Executing
    E->>B: Exec
    B-->>E: result
    E->>E: endExec — state=Ready, armIdle (gen=g1)
    E->>K: AfterFunc(idle) → idleReached(g1)
    C-->>E: result

    Note over K: idle window passes

    K->>E: idleReached(g1)
    E->>E: opMu.Lock (waits for any command)
    E->>E: beginStop(g1): state==Ready? gen==g1? now>=dueAt?
    E->>B: Stop(context.Background())
    B-->>E: nil
    E->>E: endStop — state=Stopped, cancelIdle
    E->>C: Event{Stopped}
```

And the stale case, which is the reason `gen` exists at all:

```mermaid
sequenceDiagram
    autonumber
    participant K as Clock timer (gen g1)
    participant C as caller
    participant E as entry

    Note over K: fires, goroutine scheduled
    C->>E: Exec — takes opMu, armIdle bumps gen to g2
    K->>E: idleReached(g1) blocks on opMu
    C-->>E: command done, opMu released
    K->>E: beginStop(g1) → gen is g2 → false
    Note over K: returns silently, no Stop, no event
```

Three consequences encoded in the code:

- an idle stop uses `context.Background()` — nobody is waiting on it
  (`manager.go:266`);
- a failed stop leaves the sandbox in `Ready` and gives it **one full idle
  window** before retrying, rather than a tight loop (`endStop`, `manager.go:289`);
- `WithIdleTimeout(0)` (or negative) disables stopping entirely (`armIdle`,
  `manager.go:371`).

## 7. Reconciliation after a restart

A restarted harness starts from an empty belief. `Reconcile` replaces that with
ground truth, and **changes nothing on the backend**.

```mermaid
flowchart TD
    R["Reconcile(ctx)"] --> L["backend.List()"]
    L -->|error| ERR["ReconcileReport{}, err"]
    L --> LOOP{"for each BackendSandbox"}
    LOOP --> K{"already tracked?"}
    K -->|yes| SKIP["leave exactly as is"]
    K -->|no| S{"BackendState"}
    S -->|Running| AD["observe(Ready)<br/>→ armIdle<br/>→ report.Adopted"]
    S -->|Stopped| AS["observe(Stopped)<br/>filesystem intact<br/>→ report.Asleep"]
    AD --> EV["Event{Observed}"]
    AS --> EV
```

The load-bearing detail: compute someone left running is put on the idle clock,
which **bounds a crash's leftovers to one idle window** rather than forever
(`manager.go:126-134`, `TestReconciledSandboxStopsAfterOneIdleWindow`).

## 8. Events

`Inspect` answers "what is true now"; events answer "what just changed, as it
changes" (`observer.go:3-12`). They exist for the transitions no caller can see:
an idle stop has no caller, and a prepare can mean pulling an image long before
the first command returns.

```mermaid
flowchart LR
    P1["EnsureReady ok"] --> E1["EventPrepared<br/>Preparing → Executing"]
    P2["EnsureReady failed"] --> E2["EventPrepareFailed<br/>Preparing → prior"]
    S1["idle Stop ok"] --> E3["EventStopped<br/>Stopping → Stopped"]
    S2["idle Stop failed"] --> E4["EventStopFailed<br/>Stopping → Ready"]
    D1["Destroy ok"] --> E5["EventDestroyed<br/>Destroying → Destroyed"]
    D2["Destroy failed"] --> E6["EventDestroyFailed<br/>Destroying → prior"]
    RC["Reconcile found it"] --> E7["EventObserved<br/>Unknown → found state"]
    X["a command ran"] --> N["no event —<br/>the caller already knows"]
```

`From`/`To` mean a failure says not just *that* it failed but *where the sandbox
was left*, which is where the next command starts from.

The callback contract (`observer.go:74-88`) is the part most likely to be misused
by application code:

- it runs on the transitioning goroutine, after commit, with **no state or map
  lock held** — so it may call `Inspect`;
- but the sandbox's **`opMu` is still held, deliberately**, which is what orders
  events per sandbox. So an observer must **not** call `Exec` or `Destroy` on the
  sandbox it is being told about — that self-deadlocks;
- it must not do slow work: an idle stop waits for it, and a prepare is on the
  path of the command that triggered it.

## 9. The Docker backend

One sandbox is one long-lived container, named `metaharness-sandbox-<name>` and
labelled `metaharness.sandbox=<name>`.

```mermaid
flowchart TD
    ER["EnsureReady(spec)"] --> VN{"name matches<br/>^[a-zA-Z0-9][a-zA-Z0-9_.-]*$"}
    VN -->|no| REJ["error — names are identity,<br/>never mangled"]
    VN -->|yes| INS["ContainerInspect"]
    INS -->|NotFound| IMG{"spec.Image set?"}
    INS -->|other error| ERR2["wrap and return"]
    INS -->|nil State/base| ERR3["error: daemon reported no state"]
    INS -->|running| DONE["nil — leave it alone"]
    INS -->|stopped| START["ContainerStart"]

    IMG -->|no| ERRI["error: nothing to create it from"]
    IMG -->|yes| EI["ensureImage"]
    EI --> IINS["ImageInspect"]
    IINS -->|present| CR["ContainerCreate"]
    IINS -->|NotFound| PULL["ImagePull +<br/>drain progress stream"]
    PULL --> CR
    CR --> START
    START --> DONE
```

Design decisions and why:

| Decision | Where | Why |
| --- | --- | --- |
| Existing container is **started, never recreated** | `backend.go` `EnsureReady` | recreating takes the filesystem with it |
| Filesystem = container's writable layer | package doc | stopping costs nothing; only `Destroy` removes work |
| Keepalive `tail -f /dev/null` as `Entrypoint` | `defaultKeepalive()` | `sleep infinity` is GNU-only — busybox `sleep` exits and takes the sandbox with it. Replacing the entrypoint is deliberate: a sandbox exists to run commands on request |
| `StopTimeout: 0` at create **and** at `Stop` | `create`, `Stop` | the keepalive is PID 1 with no signal handler, so the kernel never delivers SIGTERM; a graceful timeout would always be spent in full. Setting it on the container makes even a hand-run `docker rm -f` immediate |
| Ownership = the **label**, not the name prefix | `List` | a container that merely shares the naming convention is left alone |
| `daemon` interface, not `*client.Client` | `backend.go:65` | the backend is testable without a daemon |
| Name prefix supplies Docker's required leading character | `namePrefix` | a sandbox name only has to be legal from its second character on |

Exec is the fiddliest part, because the exit status is only final once output has
ended, and because a read blocked on the daemon does not notice a context.

```mermaid
sequenceDiagram
    autonumber
    participant B as Backend.Exec
    participant D as daemon
    participant G as copy goroutine

    B->>D: ContainerExecCreate(cmd, workdir, attach out+err)
    D-->>B: execID
    B->>D: ContainerExecAttach(execID)
    D-->>B: HijackedResponse{Conn, Reader}
    B->>G: go stdcopy.StdCopy(&out, &errOut, Reader)
    par normal
        G-->>B: copied ← nil
        B->>D: ContainerExecInspect(execID)
        D-->>B: {Running:false, ExitCode:n}
        B-->>B: ExecResult{out, errOut, n}, nil
    and cancelled
        B->>B: ctx.Done()
        B->>D: attached.Close() — unblocks the read
        G-->>B: copied ← err
        B-->>B: "", "", ctx.Err()
    end
```

Two invariants here:

- `ContainerExecInspect` happens **after** the stream is drained, not alongside
  it; `Running: true` at that point is reported as an error rather than a guessed
  exit code (`executor.go:49-51`);
- on cancellation the code closes the connection *and then waits for the copy to
  return*, so the buffers are nobody else's business — this is what keeps the
  race detector quiet.

`stdcopy.StdCopy` is what separates the two streams the daemon interleaves down
one connection; tests cover output split across frames and output larger than one
read.

## 10. The local backend

`Local` and `LocalBackend` are the development path, and the doc comment is blunt
about it: **not a sandbox in any security sense** — no isolation, no limits, and
`cd ..` or an absolute path escapes `Dir` trivially (`local.go:20-26`). `Dir` only
sets where commands start.

```mermaid
flowchart LR
    subgraph LB["LocalBackend{Root}"]
        ER2["EnsureReady → MkdirAll(Root/name)"]
        EX2["Exec → Local{Dir: Root/name}.Exec"]
        ST2["Stop → nil (a directory holds no compute)"]
        DS2["Destroy → RemoveAll(Root/name)"]
        LS2["List → dirs under Root, all BackendStopped"]
    end
    ER2 --> DIR["dir(name): reject anything that is not<br/>a direct child — separators, .., absolute"]
    EX2 --> DIR
    DS2 --> DIR
```

`dir()` is the security-relevant line: a name that could point anywhere other
than a direct child of `Root` is **rejected rather than resolved**
(`local.go:133-142`, `TestLocalBackendRejectsNamesThatEscapeRoot`) — the same
posture as the Docker name regex.

## 11. Test strategy

```mermaid
flowchart TB
    subgraph unit["always on — make test (-race)"]
        M["manager_test.go 504L<br/>fakeBackend + fakeClock"]
        O["observer_test.go 349L<br/>recorder"]
        RC["reconcile_test.go 142L"]
        L["local_test.go 274L<br/>real files in t.TempDir()"]
        DB["docker/backend_test.go 439L<br/>fakeDaemon"]
        DE["docker/executor_test.go 263L<br/>real stdcopy frames"]
    end
    subgraph live["make test-docker (build tag: docker)"]
        I["docker/integration_test.go<br/>real daemon, skipped without one"]
    end

    M --> FB["fakes_test.go:<br/>call history, injectable errors,<br/>gate channels for concurrency"]
    DB --> FD["docker/fakes_test.go:<br/>container/image state, scripted execs,<br/>failOn(method), recorded StopOptions"]
```

The two seams that make this testable are worth protecting:

- **`Clock`** (`clock.go`) — the idle policy is tested by *advancing time and
  asserting*, never by sleeping. `fakeClock.Advance` runs due timers.
- **`daemon`** (`docker/backend.go:65`) — the smallest slice of the Docker SDK the
  backend needs, so the Docker backend has real unit tests instead of only
  integration ones.

Tests are named after behaviour, not methods
(`TestIdleStopReleasesComputeAndKeepsSandbox`,
`TestSandboxSurvivesStopAndWake`, `TestManagedSandboxOutlivesTheProcess`), which
matches the project's TDD rule and means the suite reads as the specification of
the contracts in §3.

## 12. Notes for the reviewer

Verified observations, roughly in descending order of how much they'd cost later.

1. **No `Manager.Close` / shutdown path.** There is no way to drain or cancel
   armed idle timers, and an idle stop runs on `context.Background()` with no
   handle to wait on (`manager.go:266`) — a process that exits mid-stop leaves
   the container running until the next `Reconcile` re-arms it. Nothing releases
   the backend either: `docker.Backend.Close()` exists (`backend.go:126`) but the
   manager never calls it and has no lifecycle of its own.
2. **`WithKeepalive`'s doc comment is stale.** It says "the default,
   `sleep infinity`, is not in every image", but the default is
   `tail -f /dev/null` (`defaultKeepalive()`), and the package comment explains at
   length *why* it isn't `sleep infinity`. The comment contradicts the code two
   functions above it.
3. **No resource limits or network policy in the Docker backend.** `create`
   passes `&container.HostConfig{}` — default network (full egress), no memory or
   CPU cap, no pids limit, no read-only paths, and whatever user the image
   defaults to (usually root). For a component called
   "sandbox" running model-authored commands, the absent knobs are worth an
   explicit decision + doc line, even if the answer is "later".
4. **Exec output is fully buffered in memory**, in both backends
   (`bytes.Buffer` in `executor.go` and `local.go`). A command that prints a
   gigabyte takes the process with it. No cap, no truncation, no streaming.
5. **`armIdle` bumps `gen` twice** — it calls `cancelIdle` (which bumps) and then
   bumps again (`manager.go:369-379`). Harmless, since only inequality matters,
   but it reads as if one of the two increments is a leftover.
6. **`Backend.container` as a method name** (`docker/backend.go:276`) sits next to
   the imported `container` package used heavily in the same file. It resolves
   fine — methods aren't bare identifiers — but it's a coin-flip of confusion for
   the next reader, and it has exactly one caller (`containerName`).
7. **A second `Open` with a different `Image` is silently ignored.** First spec
   for a name wins (`entryFor`/`adopt`, `manager.go:154-167`). This is documented
   on `agent.SandboxSpec` and is the right semantics, but it is silent — no event,
   no error — so a caller who bumped the image and sees the old one has nothing to
   go on.
8. **`Reconcile` is opt-in and undocumented outside the code.** Nothing in
   `README.md`/`STACK.md` tells an application author that they must call it at
   startup, and skipping it means leaked compute stays leaked. Worth a line in the
   package doc or STACK.md.
9. **`LocalBackend.List` reports everything as `BackendStopped`**, so a reconciled
   local sandbox is always `Asleep`. Correct (a directory has no compute) but it
   means the `Adopted` path is only ever exercised against Docker and fakes.
10. **`handle` has no reference counting** — `Close` sets a flag and nothing else,
    by design (`handle.go:15,28-31`). So nothing anywhere knows how many callers
    are live; idle time is the only signal. That's a deliberate simplification and
    the tests pin it (`TestCloseIsLifecycleNeutral`,
    `TestHandlesShareOneSandbox`) — just be aware there is no "last user left"
    hook to hang anything on.
