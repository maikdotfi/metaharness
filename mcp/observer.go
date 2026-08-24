package mcp

import "time"

// Event reports what a server did. It exists for what a consumer cannot read in
// their own source: reflection means what reached the model came from a server,
// at runtime. Printing the catalogue needs no event — every agent.Tool answers
// Definition() — so this covers only what is otherwise invisible: the version
// that was negotiated, a tool that was skipped and why, and what each call did
// at the wire.
//
// Which fields are set depends on Type.
type Event struct {
	Type     EventType
	Server   string        // serverInfo.Name once known, else the command or endpoint
	Tool     string        // the tool, on EventSkipped and EventCalled
	Protocol string        // the negotiated version, on EventDialed
	Count    int           // tools exposed, on EventDiscovered
	Duration time.Duration // on EventCalled
	Err      error
}

// EventType is what happened.
type EventType uint8

const (
	// EventDialed: a connection was established, or failed to be. Protocol is
	// what was negotiated.
	EventDialed EventType = iota
	// EventDiscovered: tools/list answered, and Count tools were exposed.
	EventDiscovered
	// EventSkipped: a tool the server advertises was not exposed. Err says why.
	EventSkipped
	// EventCalled: a tool call finished, with Err set if it could not be made.
	// This is the only report the declared door gets, which is where it matters
	// most: a hand-written tool naming a wire tool the server no longer has
	// appears here and nowhere else.
	EventCalled
	// EventRedialed: a call found the connection gone and opened a new one. The
	// call itself then succeeded or failed as an EventCalled of its own.
	EventRedialed
)

func (t EventType) String() string {
	switch t {
	case EventDialed:
		return "dialed"
	case EventDiscovered:
		return "discovered"
	case EventSkipped:
		return "skipped"
	case EventCalled:
		return "called"
	case EventRedialed:
		return "redialed"
	default:
		return "invalid"
	}
}

// WithObserver sets the function called for each event. There is one observer;
// setting it again replaces it.
//
// A callback runs on the goroutine of whichever call produced it, so it is not
// the place for slow work: it sits on the path of a tool the model is waiting
// for. Anything expensive belongs on a channel the observer sends to.
//
// Observers cannot change what happened. There is no error to return, and
// nothing is retried on their behalf.
func WithObserver(fn func(Event)) Option {
	return func(s *Server) { s.observer = fn }
}

// report hands one event to the observer, if there is one.
func (s *Server) report(ev Event) {
	if s.observer == nil {
		return
	}
	s.observer(ev)
}
