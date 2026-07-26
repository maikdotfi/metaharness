package agent_test

import (
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// TestReconcileAdoptsARunningOrphan pins the crash-window fix: compute left
// running by a process that died before its sleep fired is picked up with a fresh
// idle deadline, so it sleeps in one window like any other sandbox instead of
// running forever.
func TestReconcileAdoptsARunningOrphan(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{Listing: []agent.SandboxInfo{
		{Name: "work", State: agent.SandboxAwake, Durable: true},
	}}
	r, clock, rec := newRegistry(t, inner)

	report, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(report.Adopted, []string{"work"}) {
		t.Errorf("report = %+v, want work adopted", report)
	}

	entry := onlyEntry(t, r)
	if entry.State != agent.SandboxAwake || entry.Refs != 0 {
		t.Errorf("entry = %+v, want an awake sandbox nobody holds", entry)
	}
	if want := regEpoch.Add(window); !entry.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want a fresh deadline at %v", entry.DueAt, want)
	}
	if got := inner.Specs(); len(got) != 1 || got[0].Name != "work" || !got[0].Durable {
		t.Errorf("backend acquired %+v, want one durable attach to work", got)
	}
	if _, ok := rec.find(agent.SandboxAdopted); !ok {
		t.Errorf("adoption was not reported, got %v", rec.types())
	}

	clock.Advance(window)
	if got := inner.Boxes()[0].Count("sleep"); got != 1 {
		t.Errorf("an adopted orphan slept %d times, want 1 — it must behave like any other", got)
	}
}

// TestReconcileTreatsAStoppedOrphanAsAsleep pins that reconciliation never wakes
// anything: a stopped durable sandbox is simply known to be asleep, and the
// backend is not touched until something actually needs to run a command.
func TestReconcileTreatsAStoppedOrphanAsAsleep(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{Listing: []agent.SandboxInfo{
		{Name: "work", State: agent.SandboxAsleep, Durable: true},
	}}
	r, clock, rec := newRegistry(t, inner)

	report, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(report.Asleep, []string{"work"}) {
		t.Errorf("report = %+v, want work known asleep", report)
	}
	if got := inner.Specs(); len(got) != 0 {
		t.Errorf("backend was touched %+v; attaching would have started it", got)
	}

	entry := onlyEntry(t, r)
	if entry.State != agent.SandboxAsleep || !entry.DueAt.IsZero() {
		t.Errorf("entry = %+v, want asleep with no armed deadline", entry)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed for a sleeping sandbox, want 0", got)
	}

	// The first command attaches and wakes it, transparently.
	box := mustAcquire(t, r, durable("work"))
	mustExec(t, box, "true")
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times, want 1 once a command needed it", got)
	}
	if got := onlyEntry(t, r).State; got != agent.SandboxAwake {
		t.Errorf("state = %q, want awake after a command", got)
	}
	if _, ok := rec.find(agent.SandboxWoke); !ok {
		t.Errorf("waking a known-asleep sandbox was not reported, got %v", rec.types())
	}
	if got := inner.Boxes()[0].Count("exec"); got != 1 {
		t.Errorf("ran %d commands, want 1", got)
	}
}

// TestReconcileReapsThrowawayLeftovers pins that nothing can reattach to an
// ephemeral sandbox by design, so a leftover one is waste and is removed.
func TestReconcileReapsThrowawayLeftovers(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{Listing: []agent.SandboxInfo{
		{Name: "sleepy_hopper", State: agent.SandboxAwake, Durable: false},
		{Name: "work", State: agent.SandboxAwake, Durable: true},
	}}
	r, _, _ := newRegistry(t, inner)

	report, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(report.Reaped, []string{"sleepy_hopper"}) {
		t.Errorf("report = %+v, want the throwaway leftover reaped", report)
	}
	if got := inner.Destroyed(); !slices.Equal(got, []string{"sleepy_hopper"}) {
		t.Errorf("backend destroyed %v, want [sleepy_hopper] — the durable one must survive", got)
	}
	if got := r.Snapshot(); len(got) != 1 || got[0].Name != "work" {
		t.Errorf("Snapshot = %+v, want only the durable sandbox tracked", got)
	}
}

// TestReconcileLeavesTrackedSandboxesAlone pins that reconciliation is safe to
// run at any time: a sandbox the registry is already using is reported and
// otherwise untouched, handles and deadline intact.
func TestReconcileLeavesTrackedSandboxesAlone(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{Listing: []agent.SandboxInfo{
		{Name: "work", State: agent.SandboxAwake, Durable: true},
	}}
	r, clock, _ := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))
	clock.Advance(time.Minute)
	mustExec(t, box, "true")
	before := onlyEntry(t, r)

	report, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(report.Known, []string{"work"}) {
		t.Errorf("report = %+v, want work reported as already known", report)
	}
	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times, want 1 — reconciling must not re-attach", got)
	}
	if got := onlyEntry(t, r); got != before {
		t.Errorf("entry changed across reconciliation:\n  %+v\n  %+v", before, got)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileWithoutBackendGroundTruth pins that a backend which cannot list —
// Local, a test fake — has nothing to diff against, so reconciliation is a
// harmless no-op rather than an error the caller has to special-case.
func TestReconcileWithoutBackendGroundTruth(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, _, _ := newRegistry(t, testutils.NopFactory{})

	report, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile against a backend that cannot list: %v", err)
	}
	if len(report.Adopted)+len(report.Asleep)+len(report.Reaped)+len(report.Known) != 0 {
		t.Errorf("report = %+v, want nothing found", report)
	}
}
