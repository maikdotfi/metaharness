package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCloseLeavesSandboxesAlone is what Close means: it releases the manager's
// own machinery and nothing else. Sandboxes outliving the process that used them
// is the point of the design, so shutting a manager down must not stop or remove
// any of them.
func TestCloseLeavesSandboxesAlone(t *testing.T) {
	m, backend, clock := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	clock.Advance(testIdle)

	if _, running := backend.alive("work"); !running {
		t.Error("Close stopped a sandbox; it must only release the manager")
	}
	for _, call := range backend.history() {
		if call == "Stop:work" || call == "Destroy:work" {
			t.Errorf("Close called %s on the backend", call)
		}
	}
}

// TestCloseReleasesTheBackend is the other half of what Close means: the
// manager was handed the backend, so the manager is what gives it back. A
// backend may hold a connection — the Docker one holds a daemon client — and
// dropping it is not a lifecycle event, which is why it belongs in the same
// shutdown that leaves every sandbox running.
func TestCloseReleasesTheBackend(t *testing.T) {
	m, backend, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))

	if backend.closes() != 0 {
		t.Fatalf("backend closed before shutdown")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := backend.closes(); got != 1 {
		t.Errorf("backend closed %d times, want 1", got)
	}
}

// TestCloseClosesTheBackendAfterDrainingWork checks the order the two halves of
// shutdown happen in: a command still running needs the backend, so the
// connection must outlive it.
func TestCloseClosesTheBackendAfterDrainingWork(t *testing.T) {
	m, backend, _ := newTestManager(t)
	inExec, release := make(chan struct{}), make(chan struct{})
	backend.hook = func(string) {
		close(inExec)
		<-release
	}

	box := mustOpen(t, m, "work")
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		mustExec(t, box)
	}()
	<-inExec

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// The command has not returned yet, so the backend it is running on must
	// still be open.
	time.Sleep(50 * time.Millisecond)
	if got := backend.closes(); got != 0 {
		t.Errorf("backend closed %d times while a command was still running", got)
	}

	close(release)
	<-execDone
	<-closed
	if got := backend.closes(); got != 1 {
		t.Errorf("backend closed %d times after shutdown, want 1", got)
	}
}

// TestCloseReportsTheBackendError checks a backend that failed to let go says so
// through the call that released it, rather than failing silently.
func TestCloseReportsTheBackendError(t *testing.T) {
	m, backend, _ := newTestManager(t)
	boom := errors.New("daemon connection")
	backend.closeErr = boom

	if err := m.Close(); !errors.Is(err, boom) {
		t.Errorf("Close: err = %v, want %v", err, boom)
	}
}

// TestCloseReleasesTheBackendOnce checks a second shutdown — a defer plus an
// explicit call — does not close a connection that is already gone.
func TestCloseReleasesTheBackendOnce(t *testing.T) {
	m, backend, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))

	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := backend.closes(); got != 1 {
		t.Errorf("backend closed %d times, want 1", got)
	}
}

// TestConcurrentCloseWaitsForShutdown checks every caller of Close gets the
// answer of the shutdown that actually ran, rather than returning early from a
// manager that is still draining.
func TestConcurrentCloseWaitsForShutdown(t *testing.T) {
	m, backend, _ := newTestManager(t)
	boom := errors.New("daemon connection")
	backend.closeErr = boom
	mustExec(t, mustOpen(t, m, "work"))

	errs := make(chan error, 2)
	for range cap(errs) {
		go func() { errs <- m.Close() }()
	}
	for range cap(errs) {
		if err := <-errs; !errors.Is(err, boom) {
			t.Errorf("Close: err = %v, want %v", err, boom)
		}
	}
	if got := backend.closes(); got != 1 {
		t.Errorf("backend closed %d times, want 1", got)
	}
}

// TestCloseWaitsForACommandInFlight checks Close is a drain and not a
// disconnect: a command already running finishes, and its caller still gets its
// result.
func TestCloseWaitsForACommandInFlight(t *testing.T) {
	m, backend, _ := newTestManager(t)
	inExec, release := make(chan struct{}), make(chan struct{})
	backend.hook = func(string) {
		close(inExec)
		<-release
	}

	box := mustOpen(t, m, "work")
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		mustExec(t, box)
	}()
	<-inExec

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while a command was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-execDone
	<-closed
}

// TestClosedManagerRejectsWork checks a closed manager stops handing out and
// running work, rather than quietly doing nothing.
func TestClosedManagerRejectsWork(t *testing.T) {
	m, _, _ := newTestManager(t)
	box := mustOpen(t, m, "work")
	mustExec(t, box)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := box.Exec(context.Background(), echo); !errors.Is(err, ErrShutdown) {
		t.Errorf("Exec after Close: err = %v, want ErrShutdown", err)
	}
	if _, err := m.Open("other"); !errors.Is(err, ErrShutdown) {
		t.Errorf("Open after Close: err = %v, want ErrShutdown", err)
	}
	if err := m.Destroy(context.Background(), "work"); !errors.Is(err, ErrShutdown) {
		t.Errorf("Destroy after Close: err = %v, want ErrShutdown", err)
	}
	if _, err := m.Reconcile(context.Background()); !errors.Is(err, ErrShutdown) {
		t.Errorf("Reconcile after Close: err = %v, want ErrShutdown", err)
	}
}

// TestCloseIsIdempotent checks shutting down twice — a defer plus an explicit
// call, say — is not an error to handle.
func TestCloseIsIdempotent(t *testing.T) {
	m, _, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))

	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseWithNothingOpenIsFine checks a manager that was never used shuts down
// as readily as a busy one.
func TestCloseWithNothingOpenIsFine(t *testing.T) {
	m, _, _ := newTestManager(t)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCloseAfterDestroyIsFine checks a sandbox that already released its own
// machinery does not confuse the shutdown that follows.
func TestCloseAfterDestroyIsFine(t *testing.T) {
	m, _, _ := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))

	if err := m.Destroy(context.Background(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
