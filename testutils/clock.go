package testutils

import (
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

// Clock is a fake agent.Clock driven by Advance instead of by the wall clock.
// Callbacks run synchronously on the goroutine that called Advance, in deadline
// order, so once Advance returns every consequence of that time passing has
// already happened — there is nothing to poll and nothing to sleep for.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	armed  []*fakeTimer
	nextID int
}

// NewClock returns a Clock reading now until it is advanced.
func NewClock(now time.Time) *Clock { return &Clock{now: now} }

var _ agent.Clock = (*Clock)(nil)

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) AfterFunc(d time.Duration, f func()) agent.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	t := &fakeTimer{clock: c, id: c.nextID, deadline: c.now.Add(d), f: f}
	c.armed = append(c.armed, t)
	return t
}

// Advance moves time forward by d, running every callback whose deadline it
// passes. A callback observes Now as its own deadline and may arm further timers,
// which fire too if they also come due within d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		next, at := c.takeDueLocked(target)
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = at
		c.mu.Unlock()

		// Outside the lock: a callback is free to arm or stop timers.
		next()
	}
}

// takeDueLocked removes and returns the earliest callback due by target.
func (c *Clock) takeDueLocked(target time.Time) (func(), time.Time) {
	best := -1
	for i, t := range c.armed {
		if t.deadline.After(target) {
			continue
		}
		if best < 0 || t.deadline.Before(c.armed[best].deadline) {
			best = i
		}
	}
	if best < 0 {
		return nil, time.Time{}
	}
	t := c.armed[best]
	c.armed = append(c.armed[:best], c.armed[best+1:]...)
	t.done = true
	return t.f, t.deadline
}

// Pending reports how many timers are still armed. It is the in-process
// leak-audit primitive: a test that has closed every handle and let the last
// sleep fire should see zero.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.armed)
}

type fakeTimer struct {
	clock    *Clock
	id       int
	deadline time.Time
	f        func()
	done     bool // fired or stopped; guarded by clock.mu
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	for i, armed := range t.clock.armed {
		if armed.id == t.id {
			t.clock.armed = append(t.clock.armed[:i], t.clock.armed[i+1:]...)
			break
		}
	}
	t.done = true
	return true
}
