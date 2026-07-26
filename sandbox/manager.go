package sandbox

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

// DefaultIdleTimeout is how long a sandbox stays running after its last command
// before the manager releases its compute.
const DefaultIdleTimeout = 5 * time.Minute

var (
	ErrNameRequired = errors.New("sandbox: a name is required")
	ErrDestroyed    = errors.New("sandbox: destroyed")
	ErrClosed       = errors.New("sandbox: handle is closed")
)

// Manager owns the lifecycle of the named sandboxes this process uses. It
// serializes work per name, releases compute that has gone idle, and wakes a
// sandbox again on the next command — all while the backend keeps the
// filesystem until someone destroys it explicitly.
type Manager struct {
	backend  Backend
	clock    Clock
	idle     time.Duration
	observer func(Event)

	// mu guards the name lookup and nothing else, so a slow backend call on one
	// sandbox never blocks opening or inspecting another.
	mu      sync.Mutex
	entries map[string]*entry
}

type Option func(*Manager)

// WithClock replaces the manager's source of time. Tests use it to advance the
// idle policy without sleeping.
func WithClock(c Clock) Option { return func(m *Manager) { m.clock = c } }

// WithIdleTimeout sets how long a sandbox may sit unused before its compute is
// released. A non-positive duration disables stopping entirely.
func WithIdleTimeout(d time.Duration) Option { return func(m *Manager) { m.idle = d } }

func NewManager(backend Backend, opts ...Option) *Manager {
	m := &Manager{
		backend: backend,
		clock:   systemClock{},
		idle:    DefaultIdleTimeout,
		entries: map[string]*entry{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

var _ agent.SandboxFactory = (*Manager)(nil)

// Open binds a handle to a name. It never calls the backend: nothing is
// created, started or looked up until the first command needs it.
func (m *Manager) Open(spec agent.SandboxSpec) (agent.Sandbox, error) {
	if spec.Name == "" {
		return nil, ErrNameRequired
	}
	return &handle{entry: m.entryFor(spec)}, nil
}

// Destroy removes the named sandbox and its filesystem. It waits for any
// command in flight, and a sandbox that is already gone — or was never created
// — is success. Handles bound to the name stop working; the name itself is free
// to use again.
func (m *Manager) Destroy(ctx context.Context, name string) error {
	if name == "" {
		return ErrNameRequired
	}
	e := m.entryFor(agent.SandboxSpec{Name: name})
	if err := e.destroy(ctx); err != nil {
		return err
	}
	m.forget(name, e)
	return nil
}

// Inspect reports what the manager believes about every sandbox it knows,
// sorted by name. It reads committed state only and never calls the backend, so
// it stays responsive while sandboxes are busy.
func (m *Manager) Inspect() []Info {
	m.mu.Lock()
	known := slices.Collect(maps.Values(m.entries))
	m.mu.Unlock()

	infos := make([]Info, 0, len(known))
	for _, e := range known {
		infos = append(infos, e.info())
	}
	slices.SortFunc(infos, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return infos
}

// Info is one sandbox as the manager currently sees it.
type Info struct {
	Name     string
	State    State
	Image    string
	LastExec time.Time
	DueAt    time.Time // when idle compute is released; zero if nothing is scheduled
}

// ReconcileReport is what a startup pass found on the backend.
type ReconcileReport struct {
	Adopted []string // running, now tracked and on the idle clock
	Asleep  []string // stopped, filesystem intact
}

// Reconcile asks the backend what survived and records it, so a restarted
// harness starts from ground truth rather than from an empty belief. It starts,
// stops and removes nothing: compute someone left running is simply put on the
// idle clock, which bounds it to one window. Sandboxes the manager already
// tracks are left exactly as they are.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	found, err := m.backend.List(ctx)
	if err != nil {
		return ReconcileReport{}, err
	}

	var report ReconcileReport
	for _, bs := range found {
		e, known := m.adopt(agent.SandboxSpec{Name: bs.Name, Image: bs.Image})
		if known {
			continue
		}
		if bs.State == BackendRunning {
			e.observe(StateReady)
			report.Adopted = append(report.Adopted, bs.Name)
		} else {
			e.observe(StateStopped)
			report.Asleep = append(report.Asleep, bs.Name)
		}
	}
	slices.Sort(report.Adopted)
	slices.Sort(report.Asleep)
	return report, nil
}

// entryFor returns the one entry for spec.Name, creating it on first sight. An
// existing entry wins: once a name is known, its spec is authoritative.
func (m *Manager) entryFor(spec agent.SandboxSpec) *entry {
	e, _ := m.adopt(spec)
	return e
}

func (m *Manager) adopt(spec agent.SandboxSpec) (e *entry, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[spec.Name]; ok {
		return e, true
	}
	e = &entry{mgr: m, spec: spec}
	m.entries[spec.Name] = e
	return e, false
}

// forget drops a destroyed sandbox from the lookup, unless the name has already
// been re-opened as a different sandbox.
func (m *Manager) forget(name string, e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[name] == e {
		delete(m.entries, name)
	}
}

// entry is the lifecycle of one named sandbox.
//
// It has two locks. opMu spans a whole operation — one command, stop or destroy
// at a time — and is held across backend calls, which is what serializes work
// per sandbox. mu guards the state below and is never held across a backend
// call, which is what keeps Inspect responsive while a sandbox is busy.
type entry struct {
	mgr  *Manager
	spec agent.SandboxSpec

	opMu sync.Mutex

	mu       sync.Mutex
	state    State
	gen      uint64 // bumped whenever the idle deadline changes
	timer    Timer
	lastExec time.Time
	dueAt    time.Time
}

// exec runs one command, making the sandbox ready first if it is not already.
func (e *entry) exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	prior, err := e.beginExec()
	if err != nil {
		return agent.ExecResult{}, err
	}
	if prior != StateReady {
		if err := e.mgr.backend.EnsureReady(ctx, e.spec); err != nil {
			e.restore(prior)
			e.emit(EventPrepareFailed, StatePreparing, prior, err)
			return agent.ExecResult{}, err
		}
		e.set(StateExecuting)
		e.emit(EventPrepared, StatePreparing, StateExecuting, nil)
	}

	res, err := e.mgr.backend.Exec(ctx, e.spec.Name, cmd)
	e.endExec()
	return res, err
}

// beginExec takes the sandbox off the idle clock and reports the stable state to
// fall back to if preparing it fails.
func (e *entry) beginExec() (State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == StateDestroyed {
		return e.state, ErrDestroyed
	}
	prior := e.state
	e.cancelIdle()
	if prior == StateReady {
		e.state = StateExecuting
	} else {
		e.state = StatePreparing
	}
	return prior, nil
}

// endExec puts the sandbox back on the idle clock. A command that failed still
// counts as use: the sandbox is running either way, and the caller decides what
// to do about the error.
func (e *entry) endExec() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = StateReady
	e.lastExec = e.mgr.clock.Now()
	e.armIdle()
}

// idleReached is the idle deadline arriving. It waits for any command in flight
// and then rechecks whether the deadline it was created for is still the current
// one, so a sandbox used in the meantime is left alone.
func (e *entry) idleReached(gen uint64) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	if !e.beginStop(gen) {
		return
	}
	// Nobody is waiting on this: stopping idle compute is background work, so
	// it gets the background context.
	err := e.mgr.backend.Stop(context.Background(), e.spec.Name)
	e.endStop(err)
	if err != nil {
		e.emit(EventStopFailed, StateStopping, StateReady, err)
		return
	}
	e.emit(EventStopped, StateStopping, StateStopped, nil)
}

func (e *entry) beginStop(gen uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state != StateReady || e.gen != gen || e.mgr.clock.Now().Before(e.dueAt) {
		return false
	}
	e.state = StateStopping
	e.timer = nil
	return true
}

// endStop keeps a sandbox that refused to stop usable, and gives it one full
// idle window before trying again rather than retrying in a tight loop.
func (e *entry) endStop(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err != nil {
		e.state = StateReady
		e.armIdle()
		return
	}
	e.state = StateStopped
	e.cancelIdle()
}

func (e *entry) destroy(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	prior, gone := e.beginDestroy()
	if gone {
		return nil
	}
	if err := e.mgr.backend.Destroy(ctx, e.spec.Name); err != nil {
		e.restore(prior)
		e.emit(EventDestroyFailed, StateDestroying, prior, err)
		return err
	}
	e.set(StateDestroyed)
	e.emit(EventDestroyed, StateDestroying, StateDestroyed, nil)
	return nil
}

func (e *entry) beginDestroy() (prior State, gone bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == StateDestroyed {
		return e.state, true
	}
	prior = e.state
	e.cancelIdle()
	e.state = StateDestroying
	return prior, false
}

// observe records what the backend was found to hold. A sandbox found running
// goes straight onto the idle clock, which is what bounds compute a crash left
// behind.
func (e *entry) observe(state State) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	e.state = state
	if state == StateReady {
		e.armIdle()
	}
	e.mu.Unlock()

	e.emit(EventObserved, StateUnknown, state, nil)
}

func (e *entry) set(state State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = state
}

// restore returns a sandbox to the stable state a failed operation left it in.
func (e *entry) restore(prior State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = prior
	if prior == StateReady {
		e.armIdle()
	}
}

// armIdle starts the countdown to releasing compute. Each arming gets a fresh
// generation so the deadline it replaces cannot act when it fires. Callers hold
// e.mu.
func (e *entry) armIdle() {
	e.cancelIdle()
	if e.mgr.idle <= 0 {
		return
	}
	e.gen++
	gen := e.gen
	e.dueAt = e.mgr.clock.Now().Add(e.mgr.idle)
	e.timer = e.mgr.clock.AfterFunc(e.mgr.idle, func() { e.idleReached(gen) })
}

// cancelIdle drops any pending deadline. Callers hold e.mu.
func (e *entry) cancelIdle() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.gen++
	e.dueAt = time.Time{}
}

func (e *entry) info() Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Info{
		Name:     e.spec.Name,
		State:    e.state,
		Image:    e.spec.Image,
		LastExec: e.lastExec,
		DueAt:    e.dueAt,
	}
}
