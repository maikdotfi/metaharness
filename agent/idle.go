package agent

import "time"

// idler decides when a sandbox is due to sleep. It is all of the sleep policy
// and none of the machinery: no goroutines, no locks, no clock — callers pass the
// current time in. Not safe for concurrent use; the registry serializes access.
//
// The rules, in full:
//   - never due while a command is in flight,
//   - due idleAfter after the last command finished, or after the sandbox woke or
//     appeared if none has,
//   - never due while already asleep,
//   - never due at all when idleAfter is not positive.
//
// The deadline is driven by command activity rather than by outstanding handles.
// A chat agent sits at zero handles between every message, and sleeping the
// instant a turn ended would pay for a wake on the next one.
type idler struct {
	idleAfter time.Duration

	// base is what the deadline is measured from: the last finish, or the last
	// wake or creation before that.
	base time.Time

	// lastExec is when a command last finished, zero until one has.
	lastExec time.Time

	inFlight int
	asleep   bool
}

func newIdler(idleAfter time.Duration, now time.Time) *idler {
	return &idler{idleAfter: idleAfter, base: now}
}

func (s *idler) execStarted(time.Time) { s.inFlight++ }

func (s *idler) execFinished(now time.Time) {
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.lastExec = now
	s.base = now
}

func (s *idler) slept() { s.asleep = true }

func (s *idler) woke(now time.Time) {
	s.asleep = false
	s.base = now
}

// dueAt reports the next sleep deadline, if there is one.
func (s *idler) dueAt() (time.Time, bool) {
	if s.asleep || s.inFlight > 0 || s.idleAfter <= 0 {
		return time.Time{}, false
	}
	return s.base.Add(s.idleAfter), true
}
