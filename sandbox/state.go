package sandbox

// State is what a manager currently believes about one named sandbox.
//
// Unknown, Ready, Stopped and Destroyed are the states a sandbox rests in. The
// others each mean one backend call is in flight: a sandbox is Preparing,
// Executing, Stopping or Destroying only for as long as that call takes.
type State uint8

const (
	// StateUnknown means the manager has not established what the backend has
	// under this name. The first command resolves it.
	StateUnknown State = iota
	StatePreparing
	StateReady
	StateExecuting
	StateStopping
	StateStopped
	// StateDestroying and StateDestroyed apply to the sandbox as a whole: the
	// filesystem goes with it, and handles bound to the name stop working.
	StateDestroying
	StateDestroyed
)

func (s State) String() string {
	switch s {
	case StateUnknown:
		return "unknown"
	case StatePreparing:
		return "preparing"
	case StateReady:
		return "ready"
	case StateExecuting:
		return "executing"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateDestroying:
		return "destroying"
	case StateDestroyed:
		return "destroyed"
	default:
		return "invalid"
	}
}
