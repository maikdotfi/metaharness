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
	ErrShutdown     = errors.New("sandbox: the manager is closed")
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

	// closing is closed by Close, and is every sandbox's signal to shut down.
	closing   chan struct{}
	closeOnce sync.Once

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
		closing: make(chan struct{}),
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
	e, err := m.entryFor(spec)
	if err != nil {
		return nil, err
	}
	return &handle{entry: e}, nil
}

// Destroy removes the named sandbox and its filesystem. It waits for any
// command in flight, and a sandbox that is already gone — or was never created
// — is success. Handles bound to the name stop working; the name itself is free
// to use again.
func (m *Manager) Destroy(ctx context.Context, name string) error {
	if name == "" {
		return ErrNameRequired
	}
	e, err := m.entryFor(agent.SandboxSpec{Name: name})
	if err != nil {
		return err
	}
	if _, err := e.ask(request{kind: reqDestroy, ctx: ctx}); err != nil {
		return err
	}
	m.forget(name, e)
	return nil
}

// Close releases the manager's own machinery: the goroutine and idle timer
// behind each sandbox. It waits for whatever is in flight to finish, so a
// command already running still returns its result to its caller.
//
// It deliberately leaves the sandboxes as they are. Outliving the process that
// used them is the point of them, and Reconcile is how the next one picks them
// up. Close is idempotent, and a closed manager takes no new work.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() { close(m.closing) })

	m.mu.Lock()
	known := slices.Collect(maps.Values(m.entries))
	m.mu.Unlock()

	for _, e := range known {
		<-e.gone
	}
	return nil
}

// Inspect reports what the manager believes about every sandbox it knows,
// sorted by name. It reads published state only and never calls the backend, so
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
	if m.shuttingDown() {
		return ReconcileReport{}, ErrShutdown
	}

	found, err := m.backend.List(ctx)
	if err != nil {
		return ReconcileReport{}, err
	}

	var report ReconcileReport
	for _, bs := range found {
		e, known, err := m.adopt(agent.SandboxSpec{Name: bs.Name, Image: bs.Image})
		if err != nil {
			return ReconcileReport{}, err
		}
		if known {
			continue
		}
		state := StateStopped
		if bs.State == BackendRunning {
			state = StateReady
		}
		if _, err := e.ask(request{kind: reqObserve, state: state}); err != nil {
			return ReconcileReport{}, err
		}
		if state == StateReady {
			report.Adopted = append(report.Adopted, bs.Name)
		} else {
			report.Asleep = append(report.Asleep, bs.Name)
		}
	}
	slices.Sort(report.Adopted)
	slices.Sort(report.Asleep)
	return report, nil
}

// entryFor returns the one entry for spec.Name, creating it on first sight. An
// existing entry wins: once a name is known, its spec is authoritative.
func (m *Manager) entryFor(spec agent.SandboxSpec) (*entry, error) {
	e, _, err := m.adopt(spec)
	return e, err
}

func (m *Manager) adopt(spec agent.SandboxSpec) (e *entry, known bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Checked under the lock Close collects entries under, so a sandbox is either
	// refused or started early enough for Close to wait for it.
	if m.shuttingDown() {
		return nil, false, ErrShutdown
	}
	if e, ok := m.entries[spec.Name]; ok {
		return e, true, nil
	}
	e = newEntry(m, spec)
	m.entries[spec.Name] = e
	go e.run()
	return e, false, nil
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

func (m *Manager) shuttingDown() bool {
	select {
	case <-m.closing:
		return true
	default:
		return false
	}
}

// reqKind is what a request asks the sandbox's goroutine to do. There is one per
// thing that can happen to a sandbox that is not simply reading its state.
type reqKind uint8

const (
	reqExec reqKind = iota
	reqIdle
	reqDestroy
	reqObserve
)

// request is one unit of work for a sandbox's goroutine. Every request the
// goroutine takes is answered exactly once, which is what lets a caller wait on
// reply with nothing to fall back on.
type request struct {
	kind  reqKind
	ctx   context.Context
	cmd   agent.Command // reqExec: the command to run
	state State         // reqObserve: what the backend was found holding
	due   time.Time     // reqIdle: the deadline this wakeup was scheduled for
	reply chan reply
}

type reply struct {
	res agent.ExecResult
	err error
}

// entry is one named sandbox's lifecycle, and the goroutine that runs it.
//
// All of the lifecycle state lives in fields only that goroutine touches, so
// there is no lock order to get wrong and no state to guard: run takes one
// request at a time, and that is what serializes work per sandbox. The only
// shared state is the snapshot Inspect reads, republished after every
// transition.
type entry struct {
	mgr  *Manager
	spec agent.SandboxSpec

	// reqs is unbuffered, so a delivered request is one the goroutine has taken
	// and will answer.
	reqs chan request

	// gone is closed once the goroutine has exited, and why is the reason it did.
	// why is written before gone is closed, so anyone who has seen gone closed
	// sees the reason too.
	gone chan struct{}
	why  error

	// Owned by run, and touched from nowhere else.
	state    State
	due      time.Time
	lastExec time.Time
	timer    Timer

	// mu guards the snapshot below. run is its only writer and never holds it
	// across a backend call, which is what keeps Inspect responsive.
	mu   sync.Mutex
	snap Info
}

func newEntry(m *Manager, spec agent.SandboxSpec) *entry {
	e := &entry{
		mgr:  m,
		spec: spec,
		reqs: make(chan request),
		gone: make(chan struct{}),
	}
	e.publish()
	return e
}

// ask hands one request to the goroutine and waits for its answer. A sandbox
// whose goroutine has already exited answers immediately, with the reason it
// exited: a destroyed sandbox is ErrDestroyed and a closed manager ErrShutdown.
func (e *entry) ask(req request) (agent.ExecResult, error) {
	req.reply = make(chan reply, 1)
	select {
	case e.reqs <- req:
	case <-e.gone:
		return agent.ExecResult{}, e.why
	}
	rep := <-req.reply
	return rep.res, rep.err
}

func (e *entry) run() {
	for {
		select {
		case req := <-e.reqs:
			if e.serve(req) {
				e.exit(ErrDestroyed)
				return
			}
		case <-e.mgr.closing:
			e.exit(ErrShutdown)
			return
		}
	}
}

// serve answers one request, and reports whether the sandbox went with it.
func (e *entry) serve(req request) (gone bool) {
	switch req.kind {
	case reqExec:
		e.doExec(req)
	case reqIdle:
		e.doIdle(req)
	case reqObserve:
		e.doObserve(req)
	case reqDestroy:
		return e.doDestroy(req)
	}
	return false
}

// exit releases what the goroutine owns. The sandbox itself is deliberately left
// alone: a manager shutting down is not a reason to stop anyone's work.
func (e *entry) exit(why error) {
	e.stopTimer()
	e.why = why
	close(e.gone)
}

// doExec runs one command, making the sandbox ready first if it is not already.
// A destroyed sandbox never gets here: its goroutine is gone, so ask answers for
// it.
func (e *entry) doExec(req request) {
	e.stopTimer()

	prior := e.state
	if prior != StateReady {
		e.commit(StatePreparing)
		if err := e.mgr.backend.EnsureReady(req.ctx, e.spec); err != nil {
			// Back to the stable state the failure left it in. That is never
			// StateReady, which is the branch that skips preparing altogether, so
			// there is no idle clock to restart here.
			e.commit(prior)
			e.emit(EventPrepareFailed, StatePreparing, prior, err)
			req.reply <- reply{err: err}
			return
		}
		e.commit(StateExecuting)
		e.emit(EventPrepared, StatePreparing, StateExecuting, nil)
	} else {
		e.commit(StateExecuting)
	}

	res, err := e.mgr.backend.Exec(req.ctx, e.spec.Name, req.cmd)

	// A command that failed still counts as use: the sandbox is running either
	// way, and the caller decides what to do about the error.
	e.lastExec = e.mgr.clock.Now()
	e.commit(StateReady)
	e.armIdle()
	req.reply <- reply{res: res, err: err}
}

// doIdle is the idle deadline arriving. A deadline a later command replaced is
// dropped rather than acted on: the sandbox has been used since, and its current
// deadline has not come due.
func (e *entry) doIdle(req request) {
	defer func() { req.reply <- reply{} }()

	if e.state != StateReady || !req.due.Equal(e.due) {
		return
	}
	e.commit(StateStopping)

	// Nobody is waiting on this: stopping idle compute is background work, so it
	// gets the background context.
	if err := e.mgr.backend.Stop(context.Background(), e.spec.Name); err != nil {
		// Keep a sandbox that refused to stop usable, and give it one full idle
		// window before trying again rather than retrying in a tight loop.
		e.commit(StateReady)
		e.armIdle()
		e.emit(EventStopFailed, StateStopping, StateReady, err)
		return
	}
	e.stopTimer()
	e.commit(StateStopped)
	e.emit(EventStopped, StateStopping, StateStopped, nil)
}

func (e *entry) doDestroy(req request) (gone bool) {
	prior := e.state
	e.stopTimer()
	e.commit(StateDestroying)

	if err := e.mgr.backend.Destroy(req.ctx, e.spec.Name); err != nil {
		e.commit(prior)
		if prior == StateReady {
			e.armIdle()
		}
		e.emit(EventDestroyFailed, StateDestroying, prior, err)
		req.reply <- reply{err: err}
		return false
	}
	e.commit(StateDestroyed)
	e.emit(EventDestroyed, StateDestroying, StateDestroyed, nil)
	req.reply <- reply{}
	return true
}

// doObserve records what the backend was found to hold. A sandbox found running
// goes straight onto the idle clock, which is what bounds compute a crash left
// behind.
func (e *entry) doObserve(req request) {
	e.commit(req.state)
	if req.state == StateReady {
		e.armIdle()
	}
	e.emit(EventObserved, StateUnknown, req.state, nil)
	req.reply <- reply{}
}

// armIdle starts the countdown to releasing compute.
//
// The deadline is its own generation: a wakeup carries the deadline it was
// scheduled for, so one that a later command replaced can be told from the
// current one by comparing the two — no shared counter, and nothing to keep in
// step. Waiting for the goroutine's answer is what makes a deadline synchronous:
// when the wakeup returns, the decision has been made and any stop has happened.
func (e *entry) armIdle() {
	e.stopTimer()
	if e.mgr.idle <= 0 {
		return
	}
	e.due = e.mgr.clock.Now().Add(e.mgr.idle)
	e.publish()

	due := e.due
	e.timer = e.mgr.clock.AfterFunc(e.mgr.idle, func() {
		e.ask(request{kind: reqIdle, due: due})
	})
}

// stopTimer takes the sandbox off the idle clock, dropping any pending wakeup
// and the deadline it was for.
func (e *entry) stopTimer() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.due = time.Time{}
	e.publish()
}

// commit records a state the sandbox has reached and publishes it for Inspect.
func (e *entry) commit(state State) {
	e.state = state
	e.publish()
}

func (e *entry) publish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snap = Info{
		Name:     e.spec.Name,
		State:    e.state,
		Image:    e.spec.Image,
		LastExec: e.lastExec,
		DueAt:    e.due,
	}
}

func (e *entry) info() Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snap
}
