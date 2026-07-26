package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestReconcileAdoptsWhatTheBackendHas checks a fresh manager learns the
// lifecycle state of sandboxes an earlier process left behind.
func TestReconcileAdoptsWhatTheBackendHas(t *testing.T) {
	m, backend, _ := newTestManager(t)
	backend.create("running-one", "test:latest", true)
	backend.create("stopped-one", "test:latest", false)

	report, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if want := []string{"running-one"}; !slices.Equal(report.Adopted, want) {
		t.Errorf("Adopted = %v, want %v", report.Adopted, want)
	}
	if want := []string{"stopped-one"}; !slices.Equal(report.Asleep, want) {
		t.Errorf("Asleep = %v, want %v", report.Asleep, want)
	}
	if got := stateOf(t, m, "running-one"); got != StateReady {
		t.Errorf("running sandbox adopted as %v, want %v", got, StateReady)
	}
	if got := stateOf(t, m, "stopped-one"); got != StateStopped {
		t.Errorf("stopped sandbox adopted as %v, want %v", got, StateStopped)
	}
}

// TestReconcileOnlyObserves checks recovery never moves anything: it reads
// ground truth and forms a belief, and the ordinary lifecycle takes it from
// there.
func TestReconcileOnlyObserves(t *testing.T) {
	m, backend, _ := newTestManager(t)
	backend.create("running-one", "test:latest", true)
	backend.create("stopped-one", "test:latest", false)

	if _, err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for _, call := range backend.history() {
		if call != "List" {
			t.Errorf("Reconcile called %s; it may only List", call)
		}
	}
	if _, running := backend.alive("running-one"); !running {
		t.Error("Reconcile stopped a running sandbox")
	}
	if exists, _ := backend.alive("stopped-one"); !exists {
		t.Error("Reconcile removed a stopped sandbox")
	}
}

// TestReconciledSandboxStopsAfterOneIdleWindow is why adoption matters: compute
// a crash left running is bounded by one idle window rather than forever.
func TestReconciledSandboxStopsAfterOneIdleWindow(t *testing.T) {
	m, backend, clock := newTestManager(t)
	backend.create("running-one", "test:latest", true)
	if _, err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	clock.Advance(testIdle)

	if _, running := backend.alive("running-one"); running {
		t.Error("an adopted running sandbox should stop one idle window after startup")
	}
}

// TestReconcileLeavesKnownSandboxesAlone checks a second pass does not reset the
// idle clock of a sandbox this process is already managing.
func TestReconcileLeavesKnownSandboxesAlone(t *testing.T) {
	m, _, clock := newTestManager(t)
	mustExec(t, mustOpen(t, m, "work"))
	due := m.Inspect()[0].DueAt

	clock.Advance(testIdle / 2)
	report, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(report.Adopted)+len(report.Asleep) != 0 {
		t.Errorf("already-known sandbox re-reported: %+v", report)
	}
	if got := m.Inspect()[0].DueAt; !got.Equal(due) {
		t.Errorf("idle deadline moved from %v to %v", due, got)
	}
}

// TestReconcileLeavesUnseenNamesUnknown checks a name the backend does not
// report is left for the first command to resolve.
func TestReconcileLeavesUnseenNamesUnknown(t *testing.T) {
	m, _, _ := newTestManager(t)
	box := mustOpen(t, m, "work")

	if _, err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := stateOf(t, m, "work"); got != StateUnknown {
		t.Fatalf("state = %v, want %v", got, StateUnknown)
	}

	mustExec(t, box)
	if got := stateOf(t, m, "work"); got != StateReady {
		t.Errorf("state after the first command = %v, want %v", got, StateReady)
	}
}

func TestReconcileReportsListFailure(t *testing.T) {
	m, backend, _ := newTestManager(t)
	backend.listErr = errors.New("daemon down")

	if _, err := m.Reconcile(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("Reconcile: err = %v, want the backend's failure", err)
	}
}

// TestReconciledSandboxIsUsable checks an adopted sandbox is a normal one: a
// command runs in it without preparing it again.
func TestReconciledSandboxIsUsable(t *testing.T) {
	m, backend, _ := newTestManager(t)
	backend.create("work", "test:latest", true)
	if _, err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustExec(t, mustOpen(t, m, "work"))

	if got := backend.count("EnsureReady"); got != 0 {
		t.Errorf("EnsureReady called %d times on an already-running sandbox, want 0", got)
	}
}
