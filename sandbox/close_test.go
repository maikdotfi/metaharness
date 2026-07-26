package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/agent"
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
	if _, err := m.Open(agent.SandboxSpec{Name: "other"}); !errors.Is(err, ErrShutdown) {
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
