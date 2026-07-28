package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// recorder collects what an observer was told, in the order it was told.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) observe(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// reset forgets what came before, so a test can assert on one phase only.
func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func (r *recorder) types() []EventType {
	var types []EventType
	for _, ev := range r.all() {
		types = append(types, ev.Type)
	}
	return types
}

// only returns the single event recorded, failing if there was not exactly one.
func (r *recorder) only(t *testing.T) Event {
	t.Helper()
	got := r.all()
	if len(got) != 1 {
		t.Fatalf("recorded %d events (%v), want exactly 1", len(got), r.types())
	}
	return got[0]
}

func watched(t *testing.T, opts ...Option) (*Manager, *fakeBackend, *fakeClock, *recorder) {
	t.Helper()
	rec := &recorder{}
	m, backend, clock := newTestManager(t, append(opts, WithObserver(rec.observe))...)
	return m, backend, clock, rec
}

// TestObserverSeesIdleStop is the reason the observer exists. Nothing calls for
// an idle stop, so without being told about it an application only learns its
// compute went away by inspecting later and noticing.
func TestObserverSeesIdleStop(t *testing.T) {
	m, _, clock, rec := watched(t)
	mustExec(t, mustOpen(t, m, "work"))
	rec.reset() // the prepare that got it running is not what this is about

	clock.Advance(testIdle)

	ev := rec.only(t)
	if ev.Type != EventStopped {
		t.Errorf("Type = %v, want %v", ev.Type, EventStopped)
	}
	if ev.Name != "work" {
		t.Errorf("Name = %q, want %q", ev.Name, "work")
	}
	if ev.From != StateStopping || ev.To != StateStopped {
		t.Errorf("transition %v -> %v, want %v -> %v", ev.From, ev.To, StateStopping, StateStopped)
	}
	if ev.Err != nil {
		t.Errorf("Err = %v, want nil", ev.Err)
	}
}

// TestObserverSeesFailedStop checks a stop that did not happen says so, and says
// where the sandbox was left: usable, on a fresh idle window.
func TestObserverSeesFailedStop(t *testing.T) {
	m, backend, clock, rec := watched(t)
	mustExec(t, mustOpen(t, m, "work"))
	rec.reset()

	boom := errors.New("daemon said no")
	backend.setStopErr(boom)
	clock.Advance(testIdle)

	ev := rec.only(t)
	if ev.Type != EventStopFailed {
		t.Errorf("Type = %v, want %v", ev.Type, EventStopFailed)
	}
	if !errors.Is(ev.Err, boom) {
		t.Errorf("Err = %v, want %v", ev.Err, boom)
	}
	if ev.From != StateStopping || ev.To != StateReady {
		t.Errorf("transition %v -> %v, want %v -> %v", ev.From, ev.To, StateStopping, StateReady)
	}
}

// TestObserverSeesPreparation checks the other slow, invisible step is reported:
// making a sandbox ready can mean pulling an image and creating a container, and
// an application may want to say so before the first command's output appears.
func TestObserverSeesPreparation(t *testing.T) {
	m, _, _, rec := watched(t)

	mustExec(t, mustOpen(t, m, "work"))

	ev := rec.only(t)
	if ev.Type != EventPrepared {
		t.Errorf("Type = %v, want %v", ev.Type, EventPrepared)
	}
	if ev.Name != "work" {
		t.Errorf("Name = %q, want %q", ev.Name, "work")
	}
	if ev.From != StatePreparing {
		t.Errorf("From = %v, want %v", ev.From, StatePreparing)
	}
}

// TestObserverSeesFailedPreparation checks a failure to get ready is reported
// with the state the sandbox fell back to, which is what the next command will
// start from.
func TestObserverSeesFailedPreparation(t *testing.T) {
	m, backend, _, rec := watched(t)
	boom := errors.New("no such image")
	backend.ensureErr = boom

	box := mustOpen(t, m, "work")
	if _, err := box.Exec(context.Background(), echo); err == nil {
		t.Fatal("Exec should have failed")
	}

	ev := rec.only(t)
	if ev.Type != EventPrepareFailed {
		t.Errorf("Type = %v, want %v", ev.Type, EventPrepareFailed)
	}
	if !errors.Is(ev.Err, boom) {
		t.Errorf("Err = %v, want %v", ev.Err, boom)
	}
	if ev.From != StatePreparing || ev.To != StateUnknown {
		t.Errorf("transition %v -> %v, want %v -> %v", ev.From, ev.To, StatePreparing, StateUnknown)
	}
}

// TestObserverSeesDestruction checks the one transition that takes the
// filesystem with it is reported, and that a failed one reports where the
// sandbox was left.
func TestObserverSeesDestruction(t *testing.T) {
	t.Run("destroyed", func(t *testing.T) {
		m, _, _, rec := watched(t)
		mustExec(t, mustOpen(t, m, "work"))
		rec.reset()

		if err := m.Destroy(context.Background(), "work"); err != nil {
			t.Fatalf("Destroy: %v", err)
		}

		ev := rec.only(t)
		if ev.Type != EventDestroyed {
			t.Errorf("Type = %v, want %v", ev.Type, EventDestroyed)
		}
		if ev.From != StateDestroying || ev.To != StateDestroyed {
			t.Errorf("transition %v -> %v, want %v -> %v", ev.From, ev.To, StateDestroying, StateDestroyed)
		}
	})

	t.Run("failed", func(t *testing.T) {
		m, backend, _, rec := watched(t)
		mustExec(t, mustOpen(t, m, "work"))
		rec.reset()

		boom := errors.New("still in use")
		backend.destroyErr = boom
		if err := m.Destroy(context.Background(), "work"); err == nil {
			t.Fatal("Destroy should have failed")
		}

		ev := rec.only(t)
		if ev.Type != EventDestroyFailed {
			t.Errorf("Type = %v, want %v", ev.Type, EventDestroyFailed)
		}
		if !errors.Is(ev.Err, boom) {
			t.Errorf("Err = %v, want %v", ev.Err, boom)
		}
		if ev.To != StateReady {
			t.Errorf("To = %v, want %v: a sandbox that survived destruction is still usable", ev.To, StateReady)
		}
	})
}

// TestObserverSeesWhatReconcileFound checks a restarted harness can report what
// it inherited as it learns it.
func TestObserverSeesWhatReconcileFound(t *testing.T) {
	m, backend, _, rec := watched(t)
	backend.create("awake", "test:latest", true)
	backend.create("asleep", "test:latest", false)

	if _, err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	found := map[string]Event{}
	for _, ev := range rec.all() {
		if ev.Type != EventObserved {
			t.Errorf("Reconcile reported %v; it only observes", ev.Type)
		}
		found[ev.Name] = ev
	}
	if len(found) != 2 {
		t.Fatalf("recorded %d observations, want 2", len(found))
	}
	if got := found["awake"]; got.To != StateReady {
		t.Errorf("awake observed as %v, want %v", got.To, StateReady)
	}
	if got := found["asleep"]; got.To != StateStopped {
		t.Errorf("asleep observed as %v, want %v", got.To, StateStopped)
	}
	if got := found["awake"]; got.From != StateUnknown {
		t.Errorf("From = %v, want %v: nothing was known before reconciling", got.From, StateUnknown)
	}
}

// TestObserverIsSilentAboutCommands pins a deliberate omission. Running a
// command on a ready sandbox is not reported: the caller is already holding the
// result, one event per tool call would drown the transitions that matter, and
// every event delivered is a callback the command waits for.
func TestObserverIsSilentAboutCommands(t *testing.T) {
	m, _, _, rec := watched(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)
	rec.reset()

	mustExec(t, box)
	mustExec(t, box)

	if got := rec.all(); len(got) != 0 {
		t.Errorf("recorded %v for commands on a ready sandbox, want nothing", rec.types())
	}
}

// TestObserverIsSilentAboutAStaleDeadline checks nothing is reported when
// nothing happened: a deadline a later command replaced fires, decides against
// stopping, and that is not a transition.
func TestObserverIsSilentAboutAStaleDeadline(t *testing.T) {
	m, _, clock, rec := watched(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	clock.Advance(testIdle / 2)
	mustExec(t, box) // replaces the deadline
	rec.reset()
	clock.Advance(testIdle / 2) // the replaced deadline comes due

	if got := rec.all(); len(got) != 0 {
		t.Errorf("recorded %v for a deadline that was replaced, want nothing", rec.types())
	}
}

// TestObserverRunsOutsideTheStateLock is invariant 12 as a test: an observer is
// handed a committed transition, so it can ask the manager what it now believes
// without deadlocking against the transition that woke it.
func TestObserverRunsOutsideTheStateLock(t *testing.T) {
	var (
		seen []State
		m    *Manager
	)
	rec := &recorder{}

	m, _, clock := newTestManager(t, WithObserver(func(ev Event) {
		rec.observe(ev)
		// Inspect needs the map lock and every entry's state lock. If either were
		// still held here, this would deadlock rather than fail.
		for _, info := range m.Inspect() {
			seen = append(seen, info.State)
		}
	}))
	mustExec(t, mustOpen(t, m, "work"))
	clock.Advance(testIdle)

	if len(rec.all()) != 2 {
		t.Fatalf("recorded %v, want a prepare and a stop", rec.types())
	}
	if want := []State{StateExecuting, StateStopped}; !slices.Equal(seen, want) {
		t.Errorf("observers saw %v, want %v: an event must arrive after its transition is committed", seen, want)
	}
}

// TestObserverSeesALifecycleInOrder checks events for one sandbox arrive in the
// order the transitions happened, which is the whole value of a log of them.
func TestObserverSeesALifecycleInOrder(t *testing.T) {
	m, _, clock, rec := watched(t)
	box := mustOpen(t, m, "work")

	mustExec(t, box)        // prepared
	clock.Advance(testIdle) // stopped
	mustExec(t, box)        // prepared again, waking it
	if err := m.Destroy(context.Background(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []EventType{EventPrepared, EventStopped, EventPrepared, EventDestroyed}
	if got := rec.types(); !slices.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
}

// TestNoObserverIsFine checks the observer is genuinely optional: a manager
// without one runs a whole lifecycle unbothered.
func TestNoObserverIsFine(t *testing.T) {
	m, _, clock := newTestManager(t)
	box := mustOpen(t, m, "work")

	mustExec(t, box)
	clock.Advance(testIdle)
	mustExec(t, box)
	if err := m.Destroy(context.Background(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

// TestEventTypeString checks events are readable in a log without the reader
// having to know the numbers.
func TestEventTypeString(t *testing.T) {
	for _, tc := range []struct {
		t    EventType
		want string
	}{
		{EventPrepared, "prepared"},
		{EventPrepareFailed, "prepare failed"},
		{EventStopped, "stopped"},
		{EventStopFailed, "stop failed"},
		{EventDestroyed, "destroyed"},
		{EventDestroyFailed, "destroy failed"},
		{EventObserved, "observed"},
		{EventReconcileFailed, "reconcile failed"},
	} {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("EventType(%d).String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}
