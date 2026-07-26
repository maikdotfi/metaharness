package agent_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"go.uber.org/goleak"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
	"github.com/maikdotfi/metaharness/tools"
)

// The registry's tests need no Docker and no real time: an inner fake sandbox
// records the calls, and a fake clock decides when deadlines pass.
const window = 15 * time.Minute

var regEpoch = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func durable(name string) agent.SandboxSpec {
	return agent.SandboxSpec{Name: name, Image: "golang:1.26", Durable: true}
}

// newRegistry wires a registry over an inner factory with simulated time and an
// event recorder.
func newRegistry(t *testing.T, inner agent.SandboxFactory) (*agent.Registry, *testutils.Clock, *recorder) {
	t.Helper()
	clock := testutils.NewClock(regEpoch)
	rec := &recorder{}
	r := agent.NewRegistry(inner,
		agent.WithIdleAfter(window),
		agent.WithClock(clock),
		agent.WithObserver(rec.observe),
	)
	return r, clock, rec
}

// mustAcquire fails the test if a sandbox cannot be acquired.
func mustAcquire(t *testing.T, r *agent.Registry, spec agent.SandboxSpec) agent.Sandbox {
	t.Helper()
	box, err := r.Acquire(t.Context(), spec)
	if err != nil {
		t.Fatalf("Acquire(%+v): %v", spec, err)
	}
	return box
}

func mustExec(t *testing.T, box agent.Sandbox, script string) {
	t.Helper()
	if _, err := box.Exec(t.Context(), bashCmd(script)); err != nil {
		t.Fatalf("Exec(%q): %v", script, err)
	}
}

// onlyEntry returns the single sandbox the registry believes in.
func onlyEntry(t *testing.T, r *agent.Registry) agent.SandboxInfo {
	t.Helper()
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot = %+v, want exactly one sandbox", snap)
	}
	return snap[0]
}

// TestRegistryRejectsDurableSandboxWithoutName pins that durability needs an
// identity: without a name there is nothing to attach to, share, or destroy.
func TestRegistryRejectsDurableSandboxWithoutName(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, _, _ := newRegistry(t, inner)

	if _, err := r.Acquire(t.Context(), agent.SandboxSpec{Durable: true}); err == nil {
		t.Fatal("expected an error for a durable spec with no name")
	}
	if got := inner.Specs(); len(got) != 0 {
		t.Errorf("a rejected spec reached the backend: %+v", got)
	}
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("a rejected spec left an entry behind: %+v", got)
	}
}

// TestRegistryPassesNonDurableSandboxesThrough pins that today's behaviour is
// untouched: every acquire makes a fresh sandbox and Close takes it away again.
func TestRegistryPassesNonDurableSandboxesThrough(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, _ := newRegistry(t, inner)

	first := mustAcquire(t, r, agent.SandboxSpec{Image: "golang:1.26"})
	second := mustAcquire(t, r, agent.SandboxSpec{Image: "golang:1.26"})

	if got := len(inner.Specs()); got != 2 {
		t.Errorf("backend acquired %d times, want one per non-durable acquire (2)", got)
	}
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("a non-durable sandbox was tracked by the registry: %+v", got)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, box := range inner.Boxes() {
		if n := box.Count("close"); n != 1 {
			t.Errorf("ephemeral sandbox %d closed %d times, want exactly 1", i, n)
		}
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed for pass-through sandboxes, want 0", got)
	}
}

// TestRegistrySharesOneSandboxPerName pins sharing: two durable acquires of one
// name reach the backend once and hand out handles onto the same sandbox, with
// one shared idle deadline — a command through either handle pushes it out.
func TestRegistrySharesOneSandboxPerName(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, _ := newRegistry(t, inner)

	a := mustAcquire(t, r, durable("work"))
	b := mustAcquire(t, r, durable("work"))

	if got := inner.Specs(); len(got) != 1 {
		t.Fatalf("backend acquired %d times for one name, want 1", len(got))
	}
	if got := onlyEntry(t, r).Refs; got != 2 {
		t.Errorf("Refs = %d, want 2 outstanding handles", got)
	}

	clock.Advance(5 * time.Minute)
	mustExec(t, a, "echo from a")

	// The deadline the registry now believes in is measured from a's command,
	// even though it is b that is about to be asked about it.
	if got, want := onlyEntry(t, r).DueAt, regEpoch.Add(5*time.Minute+window); !got.Equal(want) {
		t.Errorf("DueAt after a's command = %v, want %v", got, want)
	}

	clock.Advance(5 * time.Minute)
	mustExec(t, b, "echo from b")

	box := inner.Boxes()[0]
	if got := box.Count("exec"); got != 2 {
		t.Errorf("the shared sandbox ran %d commands, want 2", got)
	}
	if got := box.Count("sleep"); got != 0 {
		t.Errorf("the sandbox slept %d times while in use, want 0", got)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryEntryOutlivesItsHandles is the heart of the bridge's shape: the
// sandbox survives having no handles at all, sleeps on its own, and a later
// acquire reattaches and wakes it lazily. Nothing ever closes the inner handle.
func TestRegistryEntryOutlivesItsHandles(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, _ := newRegistry(t, inner)

	a := mustAcquire(t, r, durable("work"))
	b := mustAcquire(t, r, durable("work"))
	mustExec(t, a, "true")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	entry := onlyEntry(t, r)
	if entry.Refs != 0 || entry.State != agent.SandboxAwake {
		t.Fatalf("entry after closing every handle = %+v, want refs 0 and awake", entry)
	}

	clock.Advance(window)
	if got := onlyEntry(t, r).State; got != agent.SandboxAsleep {
		t.Fatalf("state after the idle window = %q, want asleep", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers still armed once asleep, want 0", got)
	}

	// Reattaching does not wake anything by itself; the next command does.
	c := mustAcquire(t, r, durable("work"))
	if got := onlyEntry(t, r).State; got != agent.SandboxAsleep {
		t.Errorf("acquiring woke the sandbox eagerly; state = %q, want asleep", got)
	}
	mustExec(t, c, "true")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	box := inner.Boxes()[0]
	want := []string{"exec", "sleep", "wake", "exec"}
	if got := box.Log(); !slices.Equal(got, want) {
		t.Errorf("sandbox calls = %v, want %v", got, want)
	}
	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times, want 1 for the whole life of the name", got)
	}
	if got := box.Count("close"); got != 0 {
		t.Errorf("the durable sandbox was closed %d times; detach must never close", got)
	}
}

// TestRegistryBridgeShapeSleepsOnceWhenIdle pins the Telegram bridge's rhythm:
// one acquire/exec/close per message, minutes apart, must not pay for a wake
// between messages — and must sleep exactly once when the conversation stops.
func TestRegistryBridgeShapeSleepsOnceWhenIdle(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, _ := newRegistry(t, inner)

	for range 4 {
		box := mustAcquire(t, r, durable("work"))
		mustExec(t, box, "true")
		if err := box.Close(); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Minute) // a human's typing pause
	}

	box := inner.Boxes()[0]
	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times across four turns, want 1", got)
	}
	if got := box.Count("sleep"); got != 0 {
		t.Errorf("slept %d times between messages, want 0", got)
	}

	clock.Advance(window)

	if got := box.Count("sleep"); got != 1 {
		t.Errorf("slept %d times after going idle, want exactly 1", got)
	}
	if got := box.Count("wake"); got != 0 {
		t.Errorf("woke %d times, want 0 — short pauses must not cost a wake", got)
	}
	if got := onlyEntry(t, r); got.State != agent.SandboxAsleep || got.Refs != 0 || !got.DueAt.IsZero() {
		t.Errorf("final entry = %+v, want asleep with no handles and no armed deadline", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed at teardown, want 0", got)
	}
}

// TestRegistryDoesNotSleepDuringACommand pins that a slow command holds off
// sleep however long it takes, and the window only starts once it finishes.
func TestRegistryDoesNotSleepDuringACommand(t *testing.T) {
	defer goleak.VerifyNone(t)
	gate := make(chan struct{})
	entered := make(chan struct{})
	inner := &testutils.SleepyFactory{Gate: gate, Entered: entered}
	r, clock, _ := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))

	done := make(chan error, 1)
	go func() {
		_, err := box.Exec(context.Background(), bashCmd("something slow"))
		done <- err
	}()
	<-entered // the command is genuinely in flight

	clock.Advance(4 * window) // long past any deadline
	if got := inner.Boxes()[0].Count("sleep"); got != 0 {
		t.Fatalf("slept %d times while a command was in flight, want 0", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed during a command, want 0 — nothing is due", got)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	clock.Advance(window - time.Minute)
	if got := inner.Boxes()[0].Count("sleep"); got != 0 {
		t.Errorf("slept %d times before the window elapsed, want 0", got)
	}
	clock.Advance(time.Minute)
	if got := inner.Boxes()[0].Count("sleep"); got != 1 {
		t.Errorf("slept %d times a full window after the command finished, want 1", got)
	}
}

// TestRegistryNeverSleepsASandboxThatCannotSleep pins that wrapping a backend
// without the capability costs nothing: no timers, no attempts, forever.
func TestRegistryNeverSleepsASandboxThatCannotSleep(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, clock, rec := newRegistry(t, testutils.NopFactory{})

	box := mustAcquire(t, r, durable("work"))
	mustExec(t, box, "true")
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	if got := clock.Pending(); got != 0 {
		t.Fatalf("%d timers armed for a sandbox that cannot sleep, want 0", got)
	}
	clock.Advance(365 * 24 * time.Hour)

	entry := onlyEntry(t, r)
	if entry.State != agent.SandboxAwake || !entry.DueAt.IsZero() {
		t.Errorf("entry = %+v, want permanently awake with no deadline", entry)
	}
	for _, ev := range rec.types() {
		if ev == agent.SandboxSlept || ev == agent.SandboxSleepFailed {
			t.Errorf("reported %q for a sandbox that cannot sleep", ev)
		}
	}
}

// TestRegistryWakeFailureSurfacesToTheCaller pins the failure path: the command
// fails with the wake error, the sandbox is still asleep, nothing ran in it, and
// the next attempt can succeed.
func TestRegistryWakeFailureSurfacesToTheCaller(t *testing.T) {
	defer goleak.VerifyNone(t)
	wakeBoom := errors.New("docker start: no space left on device")
	inner := &testutils.SleepyFactory{
		OnWake: func(n int) error {
			if n == 1 {
				return wakeBoom
			}
			return nil
		},
	}
	r, clock, rec := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))
	clock.Advance(window) // fall asleep

	_, err := box.Exec(t.Context(), bashCmd("true"))
	if !errors.Is(err, wakeBoom) {
		t.Fatalf("Exec error = %v, want the wake failure %v", err, wakeBoom)
	}
	if got := onlyEntry(t, r).State; got != agent.SandboxAsleep {
		t.Errorf("state after a failed wake = %q, want asleep", got)
	}
	if _, ok := rec.find(agent.SandboxWakeFailed); !ok {
		t.Errorf("no wake_failed reported, got %v", rec.types())
	}
	box0 := inner.Boxes()[0]
	if got := box0.Count("exec"); got != 0 {
		t.Errorf("ran %d commands in a sandbox that never woke, want 0", got)
	}

	mustExec(t, box, "true") // the retry wakes it
	if got := onlyEntry(t, r).State; got != agent.SandboxAwake {
		t.Errorf("state after a successful wake = %q, want awake", got)
	}
	if want := []string{"sleep", "wake", "wake", "exec"}; !slices.Equal(box0.Log(), want) {
		t.Errorf("sandbox calls = %v, want %v", box0.Log(), want)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRegistrySleepFailureRetriesNextPeriod pins the other failure path: a
// sandbox that would not go to sleep stays awake and usable, and is tried again
// one window later rather than being retried in a tight loop or given up on.
func TestRegistrySleepFailureRetriesNextPeriod(t *testing.T) {
	defer goleak.VerifyNone(t)
	sleepBoom := errors.New("docker stop: timeout")
	inner := &testutils.SleepyFactory{
		OnSleep: func(n int) error {
			if n == 1 {
				return sleepBoom
			}
			return nil
		},
	}
	r, clock, rec := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	clock.Advance(window)

	ev, ok := rec.find(agent.SandboxSleepFailed)
	if !ok {
		t.Fatalf("no sleep_failed reported, got %v", rec.types())
	}
	if !errors.Is(ev.Err, sleepBoom) {
		t.Errorf("sleep_failed carried %v, want %v", ev.Err, sleepBoom)
	}
	entry := onlyEntry(t, r)
	if entry.State != agent.SandboxAwake {
		t.Errorf("state after a failed sleep = %q, want awake", entry.State)
	}
	if want := regEpoch.Add(2 * window); !entry.DueAt.Equal(want) {
		t.Errorf("DueAt after a failed sleep = %v, want a retry at %v", entry.DueAt, want)
	}

	clock.Advance(window)
	if got := inner.Boxes()[0].Count("sleep"); got != 2 {
		t.Errorf("sleep attempted %d times, want a retry one window later (2)", got)
	}
	if got := onlyEntry(t, r).State; got != agent.SandboxAsleep {
		t.Errorf("state after the retry = %q, want asleep", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed once asleep, want 0", got)
	}
}

// TestRegistryReportsEveryTransition pins the observable story of one sandbox's
// life, in order, which is what an application logs or counts.
func TestRegistryReportsEveryTransition(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, rec := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(window)
	next := mustAcquire(t, r, durable("work"))
	mustExec(t, next, "true")
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}

	want := []agent.SandboxEventType{
		agent.SandboxAcquired, agent.SandboxReleased,
		agent.SandboxSlept,
		agent.SandboxAcquired, agent.SandboxWoke, agent.SandboxReleased,
	}
	if got := rec.types(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	for _, ev := range rec.events {
		if ev.Name != "work" {
			t.Errorf("event %+v is missing the sandbox name", ev)
		}
	}
}

// TestRegistryDestroyForgetsAndRemoves pins the one path that takes a durable
// sandbox away: the registry forgets it, the backend removes it, and handles
// still held cannot keep using it.
func TestRegistryDestroyForgetsAndRemoves(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	r, clock, _ := newRegistry(t, inner)

	box := mustAcquire(t, r, durable("work"))
	mustExec(t, box, "true")

	if err := r.Destroy(t.Context(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot after Destroy = %+v, want nothing", got)
	}
	if got := inner.Destroyed(); !slices.Equal(got, []string{"work"}) {
		t.Errorf("backend destroyed %v, want [work]", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed after Destroy, want 0", got)
	}
	if _, err := box.Exec(t.Context(), bashCmd("true")); err == nil {
		t.Error("a handle on a destroyed sandbox still ran a command")
	}

	// Acquiring the name again is a fresh sandbox, not the destroyed one.
	again := mustAcquire(t, r, durable("work"))
	if got := len(inner.Specs()); got != 2 {
		t.Errorf("backend acquired %d times, want a second one after Destroy", got)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryDestroyWithoutBackendSupport pins that destroying works against a
// backend with nothing to remove: the registry forgets the sandbox and does not
// invent an error.
func TestRegistryDestroyWithoutBackendSupport(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, _, _ := newRegistry(t, testutils.NopFactory{})

	box := mustAcquire(t, r, durable("work"))
	if err := r.Destroy(t.Context(), "work"); err != nil {
		t.Fatalf("Destroy against a backend that cannot destroy: %v", err)
	}
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot after Destroy = %+v, want nothing", got)
	}
	_ = box.Close()
}

// TestRegistryConcurrentCommandsOnOneName pins that a shared sandbox is safe
// under concurrent use: commands serialize onto the one sandbox, all of them run,
// and the backend is still only reached once. Run with -race.
func TestRegistryConcurrentCommandsOnOneName(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	clock := testutils.NewClock(regEpoch)
	r := agent.NewRegistry(inner, agent.WithIdleAfter(window), agent.WithClock(clock))

	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			box, err := r.Acquire(context.Background(), durable("work"))
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer box.Close()
			if _, err := box.Exec(context.Background(), bashCmd("true")); err != nil {
				t.Errorf("Exec: %v", err)
			}
		})
	}
	wg.Wait()

	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times, want 1", got)
	}
	if got := inner.Boxes()[0].Count("exec"); got != workers {
		t.Errorf("ran %d commands, want %d", got, workers)
	}
	entry := onlyEntry(t, r)
	if entry.Refs != 0 {
		t.Errorf("Refs = %d after every handle closed, want 0", entry.Refs)
	}

	clock.Advance(window)
	if got := onlyEntry(t, r).State; got != agent.SandboxAsleep {
		t.Errorf("state = %q, want asleep at teardown", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed at teardown, want 0", got)
	}
}

// TestRegistryDoubleCloseReleasesOnce pins that a handle closed twice — a defer
// plus an explicit close, say — does not drop somebody else's reference.
func TestRegistryDoubleCloseReleasesOnce(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, _, _ := newRegistry(t, &testutils.SleepyFactory{})

	a := mustAcquire(t, r, durable("work"))
	b := mustAcquire(t, r, durable("work"))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	if got := onlyEntry(t, r).Refs; got != 1 {
		t.Errorf("Refs = %d after closing one of two handles twice, want 1", got)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

// lateClock is a Clock whose timers refuse to stop: Stop reports failure and the
// callback stays available for the test to fire by hand. That is what a real
// timer does when Stop loses the race with a firing already under way, and it is
// the only way to reproduce a superseded deadline deterministically.
type lateClock struct {
	now   time.Time
	fires []func()
}

func (c *lateClock) Now() time.Time { return c.now }

func (c *lateClock) AfterFunc(_ time.Duration, f func()) agent.Timer {
	c.fires = append(c.fires, f)
	return unstoppableTimer{}
}

type unstoppableTimer struct{}

func (unstoppableTimer) Stop() bool { return false }

// TestRegistryIgnoresASupersededDeadline pins that a sandbox is never slept
// behind a command's back: a deadline that a later command already moved out is
// discarded when it fires, even though the sandbox is still "due to sleep" at
// some point.
func TestRegistryIgnoresASupersededDeadline(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	clock := &lateClock{now: regEpoch}
	r := agent.NewRegistry(inner, agent.WithIdleAfter(window), agent.WithClock(clock))

	box := mustAcquire(t, r, durable("work"))
	if len(clock.fires) == 0 {
		t.Fatal("no deadline was armed for a sandbox that can sleep")
	}
	superseded := clock.fires[0]

	clock.now = regEpoch.Add(window) // the original deadline has arrived...
	mustExec(t, box, "true")         // ...but a command landed, moving it out
	superseded()                     // and the old timer fires anyway

	if got := inner.Boxes()[0].Count("sleep"); got != 0 {
		t.Errorf("slept %d times on a superseded deadline, want 0", got)
	}
	if got := onlyEntry(t, r).State; got != agent.SandboxAwake {
		t.Errorf("state = %q, want awake", got)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRunThroughRegistryKeepsTheSandbox is the point of the whole design, seen
// from the agent loop: two runs against one durable sandbox reach the backend
// once, and the loop's `defer box.Close()` destroys nothing — it only detaches.
func TestRunThroughRegistryKeepsTheSandbox(t *testing.T) {
	defer goleak.VerifyNone(t)
	inner := &testutils.SleepyFactory{}
	clock := testutils.NewClock(regEpoch)
	r := agent.NewRegistry(inner, agent.WithIdleAfter(window), agent.WithClock(clock))

	mdl := &testutils.ScriptedModel{Replies: []fantasy.Message{
		testutils.AssistantToolCall(t, "c1", "bash", map[string]string{"cmd": "ls"}),
		testutils.AssistantText("first turn done"),
		testutils.AssistantToolCall(t, "c2", "bash", map[string]string{"cmd": "pwd"}),
		testutils.AssistantText("second turn done"),
	}}
	a := agent.New("system",
		agent.WithModel(mdl),
		agent.WithStore(&testutils.MemStore{}),
		agent.WithSandbox(r),
		agent.WithSandboxSpec(durable("work")),
		agent.WithTools(agent.Adapt(tools.Bash{})),
	)

	sess := testutils.UserSession("s1", "m", "first task")
	testutils.RunToCompletion(t, a, sess)

	clock.Advance(3 * time.Minute) // a pause between tasks, shorter than the window
	sess.Messages = append(sess.Messages, fantasy.NewUserMessage("second task"))
	testutils.RunToCompletion(t, a, sess)

	if got := len(inner.Specs()); got != 1 {
		t.Errorf("backend acquired %d times across two runs, want 1", got)
	}
	box := inner.Boxes()[0]
	if got := box.Count("close"); got != 0 {
		t.Errorf("the agent loop closed the sandbox %d times; Close must only detach", got)
	}
	if got := box.Count("exec"); got != 2 {
		t.Errorf("ran %d commands in the sandbox, want one per turn (2)", got)
	}
	if got := box.Count("sleep"); got != 0 {
		t.Errorf("slept %d times between turns, want 0", got)
	}

	entry := onlyEntry(t, r)
	if entry.Refs != 0 || entry.State != agent.SandboxAwake {
		t.Errorf("entry between runs = %+v, want refs 0 and awake", entry)
	}
	if got := sess.Sandbox; got != durable("work") {
		t.Errorf("session sandbox = %+v, want the resolved spec recorded", got)
	}

	clock.Advance(window)
	if got := box.Count("sleep"); got != 1 {
		t.Errorf("slept %d times once the conversation stopped, want 1", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Errorf("%d timers armed at teardown, want 0", got)
	}
}

// TestRegistrySnapshotIsSortedByName keeps the audit primitive stable to read
// and to assert on.
func TestRegistrySnapshotIsSortedByName(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, _, _ := newRegistry(t, &testutils.SleepyFactory{})

	for _, name := range []string{"work", "alpha", "notes"} {
		box := mustAcquire(t, r, durable(name))
		defer box.Close()
	}

	var names []string
	for _, info := range r.Snapshot() {
		names = append(names, info.Name)
	}
	if want := []string{"alpha", "notes", "work"}; !slices.Equal(names, want) {
		t.Errorf("Snapshot names = %v, want %v", names, want)
	}
}
