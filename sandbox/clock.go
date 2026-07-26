package sandbox

import "time"

// Clock is the only way sandbox code reads or waits on time. Injecting it keeps
// the idle policy testable: a test advances time and asserts, instead of
// sleeping and hoping.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending callback that can still be called off. Stop reports
// whether it stopped the callback from running.
type Timer interface {
	Stop() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
