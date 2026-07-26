package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

const testIdle = time.Minute

var echo = agent.Command{Cmd: "echo", Args: []string{"hi"}}

func newTestManager(t *testing.T, opts ...Option) (*Manager, *fakeBackend, *fakeClock) {
	t.Helper()
	backend := newFakeBackend()
	clock := newFakeClock()
	opts = append([]Option{WithClock(clock), WithIdleTimeout(testIdle)}, opts...)
	m := NewManager(backend, opts...)
	// Every test shuts its manager down, so a leaked sandbox goroutine shows up as
	// a hanging test rather than as nothing at all.
	t.Cleanup(func() { m.Close() })
	return m, backend, clock
}

func mustOpen(t *testing.T, m *Manager, name string) agent.Sandbox {
	t.Helper()
	box, err := m.Open(agent.SandboxSpec{Name: name, Image: "test:latest"})
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	t.Cleanup(func() { box.Close() })
	return box
}

func mustExec(t *testing.T, box agent.Sandbox) agent.ExecResult {
	t.Helper()
	res, err := box.Exec(context.Background(), echo)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res
}

func stateOf(t *testing.T, m *Manager, name string) State {
	t.Helper()
	for _, info := range m.Inspect() {
		if info.Name == name {
			return info.State
		}
	}
	t.Fatalf("Inspect() knows nothing about %q", name)
	return StateUnknown
}

// TestOpenIsBackendFree pins the rule that binding a handle to a name is not a
// lifecycle event: nothing is created, started, or even looked up until a
// command actually needs the sandbox.
func TestOpenIsBackendFree(t *testing.T) {
	m, backend, _ := newTestManager(t)

	mustOpen(t, m, "work")

	if calls := backend.history(); len(calls) != 0 {
		t.Errorf("Open touched the backend: %v", calls)
	}
}

func TestOpenRequiresName(t *testing.T) {
	m, _, _ := newTestManager(t)

	if _, err := m.Open(agent.SandboxSpec{Image: "test:latest"}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("Open without a name: err = %v, want ErrNameRequired", err)
	}
}

// TestFirstExecPreparesSandbox covers the lazy creation path: the first command
// is what makes the named sandbox exist and run.
func TestFirstExecPreparesSandbox(t *testing.T) {
	m, backend, _ := newTestManager(t)
	box := mustOpen(t, m, "work")

	res := mustExec(t, box)

	if res.Stdout != "echo hi" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "echo hi")
	}
	if _, running := backend.alive("work"); !running {
		t.Error("first Exec should have left the sandbox running")
	}
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Errorf("state after Exec = %v, want %v", got, StateReady)
	}
}

// TestExecReusesReadySandbox checks the manager does not re-prepare a sandbox it
// already believes is running.
func TestExecReusesReadySandbox(t *testing.T) {
	m, backend, _ := newTestManager(t)
	box := mustOpen(t, m, "work")

	mustExec(t, box)
	mustExec(t, box)

	if got := backend.count("EnsureReady"); got != 1 {
		t.Errorf("EnsureReady called %d times, want 1: %v", got, backend.history())
	}
}

// TestIdleStopReleasesComputeAndKeepsSandbox is the point of the whole design:
// going idle gives back the compute but must never take the filesystem with it.
func TestIdleStopReleasesComputeAndKeepsSandbox(t *testing.T) {
	m, backend, clock := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	clock.Advance(testIdle)

	exists, running := backend.alive("work")
	if !exists {
		t.Fatal("going idle destroyed the sandbox; it must only stop")
	}
	if running {
		t.Error("sandbox is still running one idle window after its last command")
	}
	if got := stateOf(t, m, "work"); got != StateStopped {
		t.Errorf("state = %v, want %v", got, StateStopped)
	}
}

// TestExecWakesStoppedSandbox checks a command after an idle stop transparently
// starts the same sandbox again.
func TestExecWakesStoppedSandbox(t *testing.T) {
	m, backend, clock := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)
	clock.Advance(testIdle)

	mustExec(t, box)

	if _, running := backend.alive("work"); !running {
		t.Error("Exec on a stopped sandbox should have woken it")
	}
	if got := backend.count("EnsureReady"); got != 2 {
		t.Errorf("EnsureReady called %d times, want 2 (create then wake): %v", got, backend.history())
	}
}

// TestExecRefreshesIdleDeadline checks that a command restarts the idle window
// and that the deadline it replaced cannot stop the sandbox when it fires.
func TestExecRefreshesIdleDeadline(t *testing.T) {
	m, backend, clock := newTestManager(t)
	box := mustOpen(t, m, "work")

	mustExec(t, box)
	clock.Advance(40 * time.Second)
	mustExec(t, box) // deadline moves to t+100s

	clock.Advance(40 * time.Second) // t+80s: the first deadline comes due, stale
	if _, running := backend.alive("work"); !running {
		t.Fatal("a superseded idle deadline stopped a sandbox used 40s ago")
	}

	clock.Advance(30 * time.Second) // t+110s: the current deadline is due
	if _, running := backend.alive("work"); running {
		t.Error("sandbox should have stopped one idle window after its last command")
	}
}

// TestZeroIdleTimeoutDisablesStopping checks that turning the idle policy off
// leaves compute running, without disturbing anything else.
func TestZeroIdleTimeoutDisablesStopping(t *testing.T) {
	m, backend, clock := newTestManager(t, WithIdleTimeout(0))
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	clock.Advance(24 * time.Hour)

	if _, running := backend.alive("work"); !running {
		t.Error("a non-positive idle timeout must disable automatic stopping")
	}
}

// TestCloseIsLifecycleNeutral checks that handles are just references: dropping
// one leaves the sandbox exactly as it was.
func TestCloseIsLifecycleNeutral(t *testing.T) {
	m, backend, _ := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	if err := box.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got: %v", err)
	}

	if _, running := backend.alive("work"); !running {
		t.Error("closing a handle stopped the sandbox")
	}
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Errorf("state after Close = %v, want %v", got, StateReady)
	}
}

func TestClosedHandleRejectsExec(t *testing.T) {
	m, _, _ := newTestManager(t)
	box := mustOpen(t, m, "work")
	box.Close()

	if _, err := box.Exec(context.Background(), echo); !errors.Is(err, ErrClosed) {
		t.Fatalf("Exec on a closed handle: err = %v, want ErrClosed", err)
	}
}

// TestHandlesShareOneSandbox checks a name identifies one sandbox, however many
// handles are open on it.
func TestHandlesShareOneSandbox(t *testing.T) {
	m, backend, _ := newTestManager(t)
	first := mustOpen(t, m, "work")
	second := mustOpen(t, m, "work")

	mustExec(t, first)
	mustExec(t, second)

	if got := backend.count("EnsureReady"); got != 1 {
		t.Errorf("EnsureReady called %d times, want 1: %v", got, backend.history())
	}
	if got := m.Inspect(); len(got) != 1 {
		t.Errorf("Inspect() = %v, want a single entry", got)
	}
}

// TestConcurrentOpenCreatesOneEntry checks the name-to-sandbox map stays
// single-entry under a race for the same new name.
func TestConcurrentOpenCreatesOneEntry(t *testing.T) {
	m, _, _ := newTestManager(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { mustOpen(t, m, "work") })
	}
	wg.Wait()

	if got := m.Inspect(); len(got) != 1 {
		t.Errorf("Inspect() = %v, want a single entry", got)
	}
}

// TestCommandsSerializePerSandbox checks two commands never run in the same
// sandbox at once, however many handles issue them.
func TestCommandsSerializePerSandbox(t *testing.T) {
	m, backend, _ := newTestManager(t)
	release := make(chan struct{})
	backend.hook = func(string) { <-release }

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { mustOpen(t, m, "work").Exec(context.Background(), echo) })
	}
	close(release)
	wg.Wait()

	backend.mu.Lock()
	peak := backend.peak["work"]
	backend.mu.Unlock()
	if peak != 1 {
		t.Errorf("%d commands ran in one sandbox at once, want 1", peak)
	}
}

// TestSandboxesRunIndependently checks serialization is per name and not a
// manager-wide lock: a command in one sandbox must not block another.
func TestSandboxesRunIndependently(t *testing.T) {
	m, backend, _ := newTestManager(t)
	inA, doneB := make(chan struct{}), make(chan struct{})
	backend.hook = func(name string) {
		switch name {
		case "a":
			close(inA)
			select {
			case <-doneB:
			case <-time.After(2 * time.Second):
				t.Error(`"b" could not run while "a" was busy: sandboxes are not independent`)
			}
		case "b":
			close(doneB)
		}
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		mustOpen(t, m, "a").Exec(context.Background(), echo)
	}()

	<-inA
	mustExec(t, mustOpen(t, m, "b"))
	<-finished
}

// TestInspectStaysResponsiveDuringCommand checks reading state never waits on a
// backend call, and reports the command in flight.
func TestInspectStaysResponsiveDuringCommand(t *testing.T) {
	m, backend, _ := newTestManager(t)
	inExec, release := make(chan struct{}), make(chan struct{})
	backend.hook = func(string) {
		close(inExec)
		<-release
	}

	go mustOpen(t, m, "work").Exec(context.Background(), echo)
	<-inExec

	if got := stateOf(t, m, "work"); got != StateExecuting {
		t.Errorf("state during a command = %v, want %v", got, StateExecuting)
	}
	close(release)
}

// TestPrepareFailureRestoresPriorState checks a sandbox that could not be made
// ready reports the failure and stays where it was, so the next command retries
// from a clean state.
func TestPrepareFailureRestoresPriorState(t *testing.T) {
	m, backend, _ := newTestManager(t)
	backend.ensureErr = errors.New("no capacity")
	box := mustOpen(t, m, "work")

	if _, err := box.Exec(context.Background(), echo); err == nil {
		t.Fatal("Exec should fail when the sandbox cannot be prepared")
	}
	if got := stateOf(t, m, "work"); got != StateUnknown {
		t.Errorf("state after a failed prepare = %v, want %v", got, StateUnknown)
	}

	backend.ensureErr = nil
	mustExec(t, box)
}

// TestCommandFailureReturnsToReady checks a failed command is not a lifecycle
// failure: the sandbox is still ready and still on the idle clock.
func TestCommandFailureReturnsToReady(t *testing.T) {
	m, backend, clock := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	backend.execErr = errors.New("stream broke")
	if _, err := box.Exec(context.Background(), echo); err == nil {
		t.Fatal("Exec should return the backend's error")
	}
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Fatalf("state after a failed command = %v, want %v", got, StateReady)
	}

	clock.Advance(testIdle)
	if got := stateOf(t, m, "work"); got != StateStopped {
		t.Errorf("a failed command should still start the idle clock; state = %v", got)
	}
}

// TestStopFailureRetriesAfterOneWindow checks a sandbox that refused to stop is
// still usable and gets tried again a full window later, not in a tight loop.
func TestStopFailureRetriesAfterOneWindow(t *testing.T) {
	m, backend, clock := newTestManager(t)
	backend.setStopErr(errors.New("daemon busy"))
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	clock.Advance(testIdle)
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Fatalf("state after a failed stop = %v, want %v", got, StateReady)
	}

	clock.Advance(testIdle / 2)
	if got := backend.count("Stop"); got != 1 {
		t.Errorf("Stop attempted %d times before the retry window elapsed, want 1", got)
	}

	backend.setStopErr(nil)
	clock.Advance(testIdle / 2)
	if _, running := backend.alive("work"); running {
		t.Error("the retried stop should have released the compute")
	}
}

// TestDestroyRemovesSandboxAndInvalidatesHandles checks destruction is the one
// operation that takes the filesystem with it, and that it is final for handles
// already bound to the name.
func TestDestroyRemovesSandboxAndInvalidatesHandles(t *testing.T) {
	m, backend, _ := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	if err := m.Destroy(context.Background(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if exists, _ := backend.alive("work"); exists {
		t.Error("Destroy left the sandbox on the backend")
	}
	if _, err := box.Exec(context.Background(), echo); !errors.Is(err, ErrDestroyed) {
		t.Errorf("Exec on a destroyed sandbox: err = %v, want ErrDestroyed", err)
	}
	if got := m.Inspect(); len(got) != 0 {
		t.Errorf("Inspect() = %v, want nothing after Destroy", got)
	}
}

// TestDestroyIsIdempotent checks destroying a name the manager never heard of,
// or destroying twice, is success rather than an error to handle.
func TestDestroyIsIdempotent(t *testing.T) {
	m, _, _ := newTestManager(t)

	if err := m.Destroy(context.Background(), "ghost"); err != nil {
		t.Fatalf("destroying an unknown sandbox: %v", err)
	}
	if err := m.Destroy(context.Background(), "ghost"); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
}

// TestOpenAfterDestroyStartsFresh checks the name is reusable once destroyed.
func TestOpenAfterDestroyStartsFresh(t *testing.T) {
	m, backend, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))
	if err := m.Destroy(context.Background(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	mustExec(t, mustOpen(t, m, "work"))

	if _, running := backend.alive("work"); !running {
		t.Error("a re-opened name should create a new sandbox")
	}
}

// TestDestroyWaitsForRunningCommand checks destruction queues behind a command
// in flight instead of pulling the sandbox out from under it. The hook holds the
// command open until Destroy has been asked for, so the two genuinely overlap.
func TestDestroyWaitsForRunningCommand(t *testing.T) {
	m, backend, _ := newTestManager(t)
	inExec, destroying := make(chan struct{}), make(chan struct{})
	backend.hook = func(string) {
		close(inExec)
		<-destroying
	}

	box := mustOpen(t, m, "work")
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		mustExec(t, box)
	}()
	<-inExec

	destroyErr := make(chan error, 1)
	go func() {
		close(destroying)
		destroyErr <- m.Destroy(context.Background(), "work")
	}()

	<-execDone
	if err := <-destroyErr; err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if n := backend.observed("Destroy"); n != 0 {
		t.Errorf("Destroy ran with %d commands still in flight, want 0", n)
	}
}

// TestDestroyFailureKeepsSandboxUsable checks a backend that refused to destroy
// leaves the manager's belief intact rather than orphaning the entry.
func TestDestroyFailureKeepsSandboxUsable(t *testing.T) {
	m, backend, _ := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)
	backend.destroyErr = errors.New("in use")

	if err := m.Destroy(context.Background(), "work"); err == nil {
		t.Fatal("Destroy should report the backend's failure")
	}
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Errorf("state after a failed destroy = %v, want %v", got, StateReady)
	}

	mustExec(t, box)
}

// TestInspectReportsKnownSandboxesSorted checks Inspect is a stable, ordered
// view a caller can print.
func TestInspectReportsKnownSandboxesSorted(t *testing.T) {
	m, _, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "beta"))
	mustOpen(t, m, "alpha")

	var names []string
	for _, info := range m.Inspect() {
		names = append(names, info.Name)
	}

	if want := []string{"alpha", "beta"}; !slices.Equal(names, want) {
		t.Errorf("Inspect() names = %v, want %v", names, want)
	}
}
