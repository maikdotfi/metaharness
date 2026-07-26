package testutils_test

import (
	"slices"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/testutils"
)

var epoch = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// TestClockAdvanceFiresDueTimersSynchronously pins the property every
// deterministic test above this leans on: once Advance returns, everything it
// caused has already happened. Nothing to poll, nothing to sleep for.
func TestClockAdvanceFiresDueTimersSynchronously(t *testing.T) {
	c := testutils.NewClock(epoch)
	fired := false

	c.AfterFunc(time.Minute, func() { fired = true })
	if fired {
		t.Fatal("timer fired before its deadline")
	}

	c.Advance(30 * time.Second)
	if fired {
		t.Fatal("timer fired half a minute early")
	}
	c.Advance(30 * time.Second)
	if !fired {
		t.Fatal("timer did not fire once its deadline passed")
	}
}

// TestClockFiresInDeadlineOrder pins that simulated time behaves like time:
// callbacks run in the order their deadlines fall, not the order they were armed.
func TestClockFiresInDeadlineOrder(t *testing.T) {
	c := testutils.NewClock(epoch)
	var order []string

	c.AfterFunc(3*time.Minute, func() { order = append(order, "third") })
	c.AfterFunc(time.Minute, func() { order = append(order, "first") })
	c.AfterFunc(2*time.Minute, func() { order = append(order, "second") })

	c.Advance(5 * time.Minute)

	if want := []string{"first", "second", "third"}; !slices.Equal(order, want) {
		t.Errorf("fired %v, want %v", order, want)
	}
}

// TestClockNowInsideCallbackIsTheDeadline pins that a callback sees the time its
// own deadline fell on, so code that re-bases a deadline from Now stays honest.
func TestClockNowInsideCallbackIsTheDeadline(t *testing.T) {
	c := testutils.NewClock(epoch)
	var seen time.Time

	c.AfterFunc(time.Minute, func() { seen = c.Now() })
	c.Advance(10 * time.Minute)

	if want := epoch.Add(time.Minute); !seen.Equal(want) {
		t.Errorf("Now inside the callback = %v, want its deadline %v", seen, want)
	}
	if want := epoch.Add(10 * time.Minute); !c.Now().Equal(want) {
		t.Errorf("Now after Advance = %v, want %v", c.Now(), want)
	}
}

// TestClockPendingCountsArmedTimers pins the leak-audit primitive: Pending is
// zero exactly when nothing is left waiting to happen.
func TestClockPendingCountsArmedTimers(t *testing.T) {
	c := testutils.NewClock(epoch)

	if got := c.Pending(); got != 0 {
		t.Fatalf("Pending on a fresh clock = %d, want 0", got)
	}
	a := c.AfterFunc(time.Minute, func() {})
	c.AfterFunc(2*time.Minute, func() {})
	if got := c.Pending(); got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}

	if !a.Stop() {
		t.Error("Stop on an armed timer = false, want true")
	}
	if got := c.Pending(); got != 1 {
		t.Fatalf("Pending after a stop = %d, want 1", got)
	}

	c.Advance(time.Hour)
	if got := c.Pending(); got != 0 {
		t.Fatalf("Pending after everything fired = %d, want 0", got)
	}
}

// TestClockStopPreventsFiring pins that a stopped timer stays silent and reports
// that it was too late to stop once it has fired.
func TestClockStopPreventsFiring(t *testing.T) {
	c := testutils.NewClock(epoch)
	fired := 0

	stopped := c.AfterFunc(time.Minute, func() { fired++ })
	stopped.Stop()
	live := c.AfterFunc(time.Minute, func() { fired++ })

	c.Advance(time.Hour)

	if fired != 1 {
		t.Errorf("callbacks fired %d times, want 1 (the stopped one must stay silent)", fired)
	}
	if live.Stop() {
		t.Error("Stop on an already-fired timer = true, want false")
	}
}

// TestClockCallbackCanRearm pins that re-arming from inside a callback works and
// that a newly armed timer already due in the same Advance still fires — the
// stop-and-rearm cycle the registry uses.
func TestClockCallbackCanRearm(t *testing.T) {
	c := testutils.NewClock(epoch)
	var fires []time.Time

	var arm func()
	arm = func() {
		fires = append(fires, c.Now())
		if len(fires) < 3 {
			c.AfterFunc(time.Minute, arm)
		}
	}
	c.AfterFunc(time.Minute, arm)

	c.Advance(5 * time.Minute)

	if len(fires) != 3 {
		t.Fatalf("fired %d times, want 3", len(fires))
	}
	for i, at := range fires {
		want := epoch.Add(time.Duration(i+1) * time.Minute)
		if !at.Equal(want) {
			t.Errorf("fire %d at %v, want %v", i, at, want)
		}
	}
	if got := c.Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0", got)
	}
}
