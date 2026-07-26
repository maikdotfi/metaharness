package agent

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultIdleAfter is how long a sandbox sits unused before it is put to sleep.
// The deadline is driven by commands, not by outstanding handles, and a chat
// agent has no handles between any two messages — so the window has to be
// comfortably longer than a human's typing pause, or every message would pay for
// a wake.
const DefaultIdleAfter = 15 * time.Minute

// ErrDurableNeedsName is returned for a durable spec with no name: durability
// means "attach to it by name next time", and there is nothing to attach to
// without an identity.
var ErrDurableNeedsName = errors.New("agent: durable sandbox requires a name")

// Registry makes durable sandboxes shareable and idle ones sleep. It decorates
// an inner SandboxFactory and is itself a SandboxFactory, so an application
// builds one at startup and hands it to WithSandbox; it is the process-wide
// authority on which sandboxes exist, never something built per run.
//
// All of the sleep policy lives here. Backends only answer "how do I sleep",
// never "when" — and a backend that cannot sleep at all is simply never asked.
type Registry struct {
	inner     SandboxFactory
	clock     Clock
	idleAfter time.Duration
	observe   func(SandboxEvent)

	mu      sync.Mutex
	entries map[string]*entry
}

// RegistryOption configures a Registry at construction.
type RegistryOption func(*Registry)

// WithIdleAfter sets how long a sandbox may sit without a command before it
// sleeps. A non-positive duration disables sleeping entirely.
func WithIdleAfter(d time.Duration) RegistryOption {
	return func(r *Registry) { r.idleAfter = d }
}

// WithClock replaces the system clock, which is how tests advance time instead of
// waiting for it.
func WithClock(c Clock) RegistryOption { return func(r *Registry) { r.clock = c } }

// WithObserver reports every sandbox transition. Callbacks are synchronous, so a
// test knows every consequence has landed once its trigger returns, but they are
// delivered outside the sandbox's lock so an observer can never deadlock the
// registry. The contract is therefore "fast and non-blocking".
func WithObserver(f func(SandboxEvent)) RegistryOption {
	return func(r *Registry) { r.observe = f }
}

func NewRegistry(inner SandboxFactory, opts ...RegistryOption) *Registry {
	r := &Registry{
		inner:     inner,
		clock:     systemClock{},
		idleAfter: DefaultIdleAfter,
		entries:   map[string]*entry{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

var _ SandboxFactory = (*Registry)(nil)

// Acquire returns a handle on the sandbox spec names. Durable specs with the same
// name share one sandbox — the inner factory is reached once per name — and a
// non-durable spec passes straight through as a fresh throwaway sandbox.
//
// Acquiring never wakes a sleeping sandbox; the next command does.
func (r *Registry) Acquire(ctx context.Context, spec SandboxSpec) (Sandbox, error) {
	if !spec.Durable {
		return r.inner.Acquire(ctx, spec)
	}
	if spec.Name == "" {
		return nil, ErrDurableNeedsName
	}

	e, err := r.entryFor(ctx, spec)
	if err != nil {
		return nil, err
	}
	refs := e.retain()
	r.emit(SandboxEvent{Type: SandboxAcquired, Name: spec.Name, Refs: refs})
	return &sandboxHandle{entry: e}, nil
}

// entryFor returns the shared entry for spec.Name, reaching the backend only if
// this is the first time the name is seen. The registry lock is held across that
// call so the "once per name" promise holds without a second placeholder state.
func (r *Registry) entryFor(ctx context.Context, spec SandboxSpec) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[spec.Name]; ok {
		return e, nil
	}
	box, err := r.inner.Acquire(ctx, spec)
	if err != nil {
		return nil, err
	}
	return r.trackLocked(spec, box, SandboxAwake), nil
}

// trackLocked starts tracking a sandbox and arms its first deadline. A nil box
// means "not attached yet": the sandbox is known to be asleep and the handle is
// fetched when something first needs to run a command.
func (r *Registry) trackLocked(spec SandboxSpec, box Sandbox, state SandboxState) *entry {
	if e, ok := r.entries[spec.Name]; ok {
		return e
	}
	e := &entry{
		reg:   r,
		spec:  spec,
		box:   box,
		idle:  newIdler(r.idleAfter, r.clock.Now()),
		state: state,
	}
	if box != nil {
		e.sleeper, _ = box.(Sleeper)
	}
	if state == SandboxAsleep {
		e.idle.slept()
	}
	r.entries[spec.Name] = e

	e.mu.Lock()
	e.armLocked()
	e.mu.Unlock()
	return e
}

// Snapshot reports what the registry believes about every sandbox it tracks,
// sorted by name. It is the leak-audit primitive: tests assert invariants on it,
// a bridge's /status reads it, and a future `sandbox ls` would read the same.
func (r *Registry) Snapshot() []SandboxInfo {
	r.mu.Lock()
	entries := slices.Collect(maps.Values(r.entries))
	r.mu.Unlock()

	infos := make([]SandboxInfo, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, e.info())
	}
	slices.SortFunc(infos, func(a, b SandboxInfo) int { return strings.Compare(a.Name, b.Name) })
	return infos
}

// Destroy forgets a sandbox and removes it from the backend, which is the only
// thing that takes a durable sandbox away. Handles still held stop working.
// Backends with nothing to remove (Local, fakes) need not implement Destroyer.
func (r *Registry) Destroy(ctx context.Context, name string) error {
	r.mu.Lock()
	e := r.entries[name]
	delete(r.entries, name)
	r.mu.Unlock()

	if e != nil {
		e.destroy()
	}
	if d, ok := r.inner.(Destroyer); ok {
		return d.Destroy(ctx, name)
	}
	return nil
}

// ReconcileReport is what one reconciliation found and did, so the caller can log
// it. Each field holds sandbox names.
type ReconcileReport struct {
	Adopted []string // running durable orphans now tracked and awake
	Asleep  []string // stopped durable orphans now known to be asleep
	Reaped  []string // throwaway leftovers removed
	Known   []string // already tracked, left alone
}

// Reconcile diffs the registry's belief against the backend's ground truth. It is
// what closes the crash window: compute left running by a process that died
// before a sleep fired is adopted with a fresh idle deadline, so it sleeps in one
// window like any other sandbox; a stopped durable sandbox is simply known to be
// asleep; and a throwaway leftover is reaped, because nothing can reattach to one
// by design.
//
// A backend that cannot list has no ground truth to diff against, so
// reconciliation reports nothing and succeeds.
func (r *Registry) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport

	lister, ok := r.inner.(Lister)
	if !ok {
		return report, nil
	}
	found, err := lister.List(ctx)
	if err != nil {
		return report, err
	}

	var errs []error
	for _, info := range found {
		spec := SandboxSpec{Name: info.Name, Durable: true}
		switch {
		case !info.Durable:
			if err := r.reap(ctx, info.Name); err != nil {
				errs = append(errs, err)
				continue
			}
			report.Reaped = append(report.Reaped, info.Name)

		case r.tracks(info.Name):
			report.Known = append(report.Known, info.Name)

		case info.State == SandboxAwake:
			box, err := r.inner.Acquire(ctx, spec)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			r.track(spec, box, SandboxAwake)
			r.emit(SandboxEvent{Type: SandboxAdopted, Name: info.Name})
			report.Adopted = append(report.Adopted, info.Name)

		default:
			// Attaching would start it, and nothing needs it started yet.
			r.track(spec, nil, SandboxAsleep)
			r.emit(SandboxEvent{Type: SandboxAdopted, Name: info.Name})
			report.Asleep = append(report.Asleep, info.Name)
		}
	}
	return report, errors.Join(errs...)
}

func (r *Registry) reap(ctx context.Context, name string) error {
	if d, ok := r.inner.(Destroyer); ok {
		return d.Destroy(ctx, name)
	}
	return nil
}

func (r *Registry) tracks(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[name]
	return ok
}

func (r *Registry) track(spec SandboxSpec, box Sandbox, state SandboxState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trackLocked(spec, box, state)
}

func (r *Registry) emit(ev SandboxEvent) {
	if r.observe != nil {
		r.observe(ev)
	}
}

// entry is one durable sandbox: one mutex, one idler, one timer, however many
// handles. Its lock both serializes the sleep/wake transition against commands
// and is what makes that transition race-free by construction.
type entry struct {
	reg  *Registry
	spec SandboxSpec

	// execMu serializes commands on this sandbox. It is separate from mu so a
	// long-running command does not hold the state lock, which would let a
	// synchronous observer or timer callback wait on it.
	execMu sync.Mutex

	mu      sync.Mutex
	box     Sandbox // nil until attached; a known-asleep sandbox attaches lazily
	sleeper Sleeper // nil when the backend cannot sleep
	idle    *idler
	state   SandboxState
	refs    int
	dead    bool

	// gen invalidates timer callbacks. Re-arming is always stop plus a fresh
	// AfterFunc, so a firing that slipped past the stop is discarded because it
	// carries an older generation.
	gen   uint64
	timer Timer
	due   time.Time // the armed deadline, zero when no timer is armed
}

func (e *entry) retain() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refs++
	return e.refs
}

func (e *entry) release() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.refs > 0 {
		e.refs--
	}
	return e.refs
}

func (e *entry) info() SandboxInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return SandboxInfo{
		Name:     e.spec.Name,
		State:    e.state,
		Durable:  true,
		Refs:     e.refs,
		LastExec: e.idle.lastExec,
		DueAt:    e.due,
	}
}

// exec runs one command, waking the sandbox first if it is asleep. Callers and
// tools never learn that any of this happened.
func (e *entry) exec(ctx context.Context, cmd Command) (ExecResult, error) {
	e.execMu.Lock()
	defer e.execMu.Unlock()

	box, err := e.begin(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	res, execErr := box.Exec(ctx, cmd)
	e.finish()
	return res, execErr
}

// begin makes the sandbox ready to run a command and stops its sleep deadline:
// nothing is due while a command is in flight.
func (e *entry) begin(ctx context.Context) (Sandbox, error) {
	e.mu.Lock()

	if e.dead {
		e.mu.Unlock()
		return nil, errors.New("agent: sandbox " + e.spec.Name + " was destroyed")
	}

	woke := false
	switch {
	case e.box == nil:
		// Known asleep and never attached: getting the handle is the wake.
		box, err := e.reg.inner.Acquire(ctx, e.spec)
		if err != nil {
			e.mu.Unlock()
			e.reg.emit(SandboxEvent{Type: SandboxWakeFailed, Name: e.spec.Name, Err: err})
			return nil, err
		}
		e.box = box
		e.sleeper, _ = box.(Sleeper)
		e.idle.woke(e.reg.clock.Now())
		e.state, woke = SandboxAwake, true

	case e.state == SandboxAsleep:
		if err := e.sleeper.Wake(ctx); err != nil {
			e.mu.Unlock()
			e.reg.emit(SandboxEvent{Type: SandboxWakeFailed, Name: e.spec.Name, Err: err})
			return nil, err
		}
		e.idle.woke(e.reg.clock.Now())
		e.state, woke = SandboxAwake, true
	}

	e.idle.execStarted(e.reg.clock.Now())
	e.disarmLocked()
	box := e.box
	refs := e.refs
	e.mu.Unlock()

	if woke {
		e.reg.emit(SandboxEvent{Type: SandboxWoke, Name: e.spec.Name, Refs: refs})
	}
	return box, nil
}

// finish records that the command is done and re-arms the sleep deadline from it.
func (e *entry) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idle.execFinished(e.reg.clock.Now())
	e.armLocked()
}

// sleepDue is the timer callback. gen is the generation it was armed with, which
// is what makes a firing that raced with a re-arm harmless.
func (e *entry) sleepDue(gen uint64) {
	e.mu.Lock()
	if e.dead || gen != e.gen || e.sleeper == nil {
		e.mu.Unlock()
		return
	}
	if _, due := e.idle.dueAt(); !due {
		e.mu.Unlock()
		return // a command landed in the meantime
	}
	e.timer, e.due = nil, time.Time{}

	// Sleeping under the lock is deliberate: no command can start mid-transition.
	// The context is the registry's own, not any caller's — sleeping is background
	// work that outlives whatever run last used the sandbox.
	var ev SandboxEvent
	if err := e.sleeper.Sleep(context.Background()); err != nil {
		// Stay awake and usable, and try again one window later rather than
		// hammering a backend that just refused.
		e.idle.woke(e.reg.clock.Now())
		e.armLocked()
		ev = SandboxEvent{Type: SandboxSleepFailed, Name: e.spec.Name, Refs: e.refs, Err: err}
	} else {
		e.idle.slept()
		e.state = SandboxAsleep
		ev = SandboxEvent{Type: SandboxSlept, Name: e.spec.Name, Refs: e.refs}
	}
	e.mu.Unlock()

	e.reg.emit(ev)
}

// armLocked arms a timer for the idler's next deadline, if there is one. A
// sandbox that cannot sleep never gets a timer at all, so wrapping Local or a
// test fake costs nothing.
func (e *entry) armLocked() {
	e.disarmLocked()
	if e.sleeper == nil {
		return
	}
	due, ok := e.idle.dueAt()
	if !ok {
		return
	}
	gen := e.gen
	e.due = due
	e.timer = e.reg.clock.AfterFunc(due.Sub(e.reg.clock.Now()), func() { e.sleepDue(gen) })
}

func (e *entry) disarmLocked() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.gen++
	e.due = time.Time{}
}

func (e *entry) destroy() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dead = true
	e.disarmLocked()
	if e.box != nil {
		_ = e.box.Close()
		e.box, e.sleeper = nil, nil
	}
}

// sandboxHandle is one reference to a shared sandbox. Closing it detaches, which
// is all it does: the sandbox stays, and sitting at zero handles is the normal
// state between two messages of a conversation.
type sandboxHandle struct {
	entry *entry
	once  sync.Once
}

var _ Sandbox = (*sandboxHandle)(nil)

func (h *sandboxHandle) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	return h.entry.exec(ctx, cmd)
}

func (h *sandboxHandle) Close() error {
	h.once.Do(func() {
		refs := h.entry.release()
		h.entry.reg.emit(SandboxEvent{
			Type: SandboxReleased, Name: h.entry.spec.Name, Refs: refs,
		})
	})
	return nil
}
