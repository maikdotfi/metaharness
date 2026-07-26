package agent

import "time"

// Clock is the only way anything in this package reads time or schedules work.
// It exists so tests can advance time instead of waiting for it: with the clock
// injected, the compiler proves no policy code reaches the system clock.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a scheduled callback that has not run yet. Stop reports whether it
// stopped the callback before it ran.
//
// There is deliberately no Reset: re-arming is always Stop plus a fresh
// AfterFunc, which sidesteps the classic Timer.Reset race — a stale firing that
// slipped past Stop is discarded by the generation it carries instead.
type Timer interface {
	Stop() bool
}

// systemClock is the real clock. It is the one legitimate caller of the time
// package's wall clock in this package; everything else takes a Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
