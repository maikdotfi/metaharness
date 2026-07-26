package sandbox

// Event is one committed lifecycle transition of one sandbox.
//
// It exists for the transitions an application cannot otherwise see. Most of a
// sandbox's life is driven by a caller who already knows what happened — the
// command they ran returned. An idle stop has no caller at all, and preparing a
// sandbox can mean pulling an image and creating a container long before the
// first command's output appears. Inspect answers "what is true now"; events
// answer "what just changed", as it changes.
//
// From and To are the states either side of the transition, so a failure says
// not just that it failed but where the sandbox was left: a sandbox that
// refused to stop is back in StateReady and still usable, and one that refused
// to be destroyed is back where it was.
type Event struct {
	Type EventType
	Name string
	From State
	To   State
	Err  error // set on the failure events, nil otherwise
}

// EventType is what happened. There is one per reported transition, so an
// observer can switch on it without knowing the transition table.
type EventType uint8

const (
	// EventPrepared: the sandbox was created or woken and is now running.
	EventPrepared EventType = iota
	// EventPrepareFailed: it could not be made ready, and the command that asked
	// for it failed. To is the state it fell back to, which is where the next
	// command starts from.
	EventPrepareFailed
	// EventStopped: an idle deadline passed and its compute was released. The
	// filesystem is untouched.
	EventStopped
	// EventStopFailed: the compute is still running, and stopping it will be
	// tried again after one more idle window.
	EventStopFailed
	// EventDestroyed: the sandbox and its filesystem are gone.
	EventDestroyed
	// EventDestroyFailed: the sandbox is still there and still usable.
	EventDestroyFailed
	// EventObserved: startup reconciliation found this sandbox on the backend.
	// To is the state it was found in.
	EventObserved
)

func (t EventType) String() string {
	switch t {
	case EventPrepared:
		return "prepared"
	case EventPrepareFailed:
		return "prepare failed"
	case EventStopped:
		return "stopped"
	case EventStopFailed:
		return "stop failed"
	case EventDestroyed:
		return "destroyed"
	case EventDestroyFailed:
		return "destroy failed"
	case EventObserved:
		return "observed"
	default:
		return "invalid"
	}
}

// WithObserver sets the function called for each committed transition. There is
// one observer; setting it again replaces it.
//
// A callback runs on the goroutine that made the transition, after it has been
// committed and with no state or map lock held, so it may call Inspect or read
// anything else the manager knows. Two things follow from that:
//
//   - it is not the place for slow work. An idle stop is background work and
//     waits for it; a prepare is on the path of the command that triggered it.
//     Anything expensive belongs on a channel the observer sends to.
//   - the sandbox's operation lock is still held, deliberately: it is what makes
//     events for one sandbox arrive in the order its transitions happened. An
//     observer must therefore not call Exec or Destroy on the sandbox it is
//     being told about, which would wait for a lock its own caller holds.
//
// Observers cannot change what happened. There is no error to return, and
// nothing is retried on their behalf.
func WithObserver(fn func(Event)) Option {
	return func(m *Manager) { m.observer = fn }
}

// emit tells the observer about a committed transition. Callers must hold no
// state lock.
func (e *entry) emit(t EventType, from, to State, err error) {
	if e.mgr.observer == nil {
		return
	}
	e.mgr.observer(Event{Type: t, Name: e.spec.Name, From: from, To: to, Err: err})
}
