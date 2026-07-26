package agent

import (
	"testing"
	"time"
)

// TestIdlerDueAt is the whole sleep policy, one rule per row. Times are literal
// and nothing here reads a clock: the idler answers "when is this sandbox due to
// sleep", and something else decides what to do about it.
func TestIdlerDueAt(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	const window = 15 * time.Minute

	tests := []struct {
		name      string
		idleAfter time.Duration
		steps     func(*idler)
		want      time.Time
		wantDue   bool
	}{
		{
			name:      "a sandbox nobody has used yet is due one window after it appeared",
			idleAfter: window,
			steps:     func(*idler) {},
			want:      at(15),
			wantDue:   true,
		},
		{
			name:      "a sandbox with a command in flight is never due",
			idleAfter: window,
			steps:     func(s *idler) { s.execStarted(at(1)) },
			wantDue:   false,
		},
		{
			name:      "the window runs from when the last command finished",
			idleAfter: window,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execFinished(at(2))
			},
			want:    at(17),
			wantDue: true,
		},
		{
			name:      "a later command pushes the deadline out",
			idleAfter: window,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execFinished(at(2))
				s.execStarted(at(3))
				s.execFinished(at(4))
			},
			want:    at(19),
			wantDue: true,
		},
		{
			name:      "overlapping commands keep it busy until the last one finishes",
			idleAfter: window,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execStarted(at(2))
				s.execFinished(at(3))
			},
			wantDue: false,
		},
		{
			name:      "a long command that outlives its own deadline is still not due",
			idleAfter: window,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execFinished(at(90))
			},
			want:    at(105),
			wantDue: true,
		},
		{
			name:      "a sleeping sandbox is never due to sleep again",
			idleAfter: window,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execFinished(at(2))
				s.slept()
			},
			wantDue: false,
		},
		{
			name:      "waking restarts the window even before any command runs",
			idleAfter: window,
			steps: func(s *idler) {
				s.slept()
				s.woke(at(10))
			},
			want:    at(25),
			wantDue: true,
		},
		{
			name:      "a command after waking takes over the deadline again",
			idleAfter: window,
			steps: func(s *idler) {
				s.slept()
				s.woke(at(10))
				s.execStarted(at(11))
				s.execFinished(at(12))
			},
			want:    at(27),
			wantDue: true,
		},
		{
			name:      "a zero window means never sleep",
			idleAfter: 0,
			steps: func(s *idler) {
				s.execStarted(at(1))
				s.execFinished(at(2))
			},
			wantDue: false,
		},
		{
			name:      "a negative window means never sleep",
			idleAfter: -time.Minute,
			steps:     func(*idler) {},
			wantDue:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newIdler(tc.idleAfter, base)
			tc.steps(s)

			got, due := s.dueAt()
			if due != tc.wantDue {
				t.Fatalf("dueAt due = %v, want %v (deadline %v)", due, tc.wantDue, got)
			}
			if due && !got.Equal(tc.want) {
				t.Errorf("dueAt = %v, want %v", got, tc.want)
			}
			if !due && !got.IsZero() {
				t.Errorf("dueAt = %v with no deadline, want the zero time", got)
			}
		})
	}
}

// TestIdlerLastExecIsTheLastFinish pins what "last used" means: a sandbox
// reports no use until a command has actually finished in it.
func TestIdlerLastExecIsTheLastFinish(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := newIdler(time.Minute, base)

	if got := s.lastExec; !got.IsZero() {
		t.Errorf("lastExec on a fresh idler = %v, want zero", got)
	}
	s.execStarted(base.Add(time.Second))
	if got := s.lastExec; !got.IsZero() {
		t.Errorf("lastExec while a command runs = %v, want zero until it finishes", got)
	}
	s.execFinished(base.Add(2 * time.Second))
	if got, want := s.lastExec, base.Add(2*time.Second); !got.Equal(want) {
		t.Errorf("lastExec = %v, want %v", got, want)
	}
}
