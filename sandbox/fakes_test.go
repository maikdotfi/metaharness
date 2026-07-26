package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

// fakeBackend models sandboxes that exist, run and stop rather than replaying
// scripted answers, so a test can assert what actually survived a lifecycle
// instead of how many times a method was poked. It also records the commands in
// flight per name, which is how the serialization tests prove that a stop or a
// destroy never lands on top of a running command.
type fakeBackend struct {
	mu    sync.Mutex
	boxes map[string]*fakeBox
	calls []string

	inflight map[string]int // commands running right now, per name
	peak     map[string]int // most commands ever running at once, per name
	sawExec  map[string]int // commands in flight when Stop/Destroy started

	ensureErr  error
	execErr    error
	stopErr    error
	destroyErr error
	listErr    error

	// hook runs inside Exec while the command counts as in flight, holding no
	// lock. Tests use it to hold a command open.
	hook func(name string)
	out  map[string]agent.ExecResult
}

type fakeBox struct {
	image   string
	running bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		boxes:    map[string]*fakeBox{},
		inflight: map[string]int{},
		peak:     map[string]int{},
		sawExec:  map[string]int{},
		out:      map[string]agent.ExecResult{},
	}
}

var _ Backend = (*fakeBackend)(nil)

func (b *fakeBackend) EnsureReady(_ context.Context, spec agent.SandboxSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, "EnsureReady:"+spec.Name)
	if b.ensureErr != nil {
		return b.ensureErr
	}
	box := b.boxes[spec.Name]
	if box == nil {
		box = &fakeBox{image: spec.Image}
		b.boxes[spec.Name] = box
	}
	box.running = true
	return nil
}

func (b *fakeBackend) Exec(_ context.Context, name string, cmd agent.Command) (agent.ExecResult, error) {
	b.mu.Lock()
	b.calls = append(b.calls, "Exec:"+name)
	switch box, ok := b.boxes[name]; {
	case b.execErr != nil:
		err := b.execErr
		b.mu.Unlock()
		return agent.ExecResult{}, err
	case !ok || !box.running:
		b.mu.Unlock()
		return agent.ExecResult{}, fmt.Errorf("fake backend: %q is not running", name)
	}
	b.inflight[name]++
	b.peak[name] = max(b.peak[name], b.inflight[name])
	res, custom := b.out[name]
	hook := b.hook
	b.mu.Unlock()

	if hook != nil {
		hook(name)
	}

	b.mu.Lock()
	b.inflight[name]--
	b.mu.Unlock()

	if !custom {
		res = agent.ExecResult{Stdout: strings.Join(append([]string{cmd.Cmd}, cmd.Args...), " ")}
	}
	return res, nil
}

func (b *fakeBackend) Stop(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, "Stop:"+name)
	b.sawExec["Stop"] = max(b.sawExec["Stop"], b.inflight[name])
	if b.stopErr != nil {
		return b.stopErr
	}
	if box := b.boxes[name]; box != nil {
		box.running = false
	}
	return nil
}

// Destroy treats a missing sandbox as success, like a real backend must for
// repeated destruction to be idempotent.
func (b *fakeBackend) Destroy(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, "Destroy:"+name)
	b.sawExec["Destroy"] = max(b.sawExec["Destroy"], b.inflight[name])
	if b.destroyErr != nil {
		return b.destroyErr
	}
	delete(b.boxes, name)
	return nil
}

func (b *fakeBackend) List(context.Context) ([]BackendSandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, "List")
	if b.listErr != nil {
		return nil, b.listErr
	}
	found := make([]BackendSandbox, 0, len(b.boxes))
	for name, box := range b.boxes {
		state := BackendStopped
		if box.running {
			state = BackendRunning
		}
		found = append(found, BackendSandbox{Name: name, Image: box.image, State: state})
	}
	slices.SortFunc(found, func(a, b BackendSandbox) int { return strings.Compare(a.Name, b.Name) })
	return found, nil
}

// create seeds a sandbox as if an earlier process had left it behind.
func (b *fakeBackend) create(name, image string, running bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.boxes[name] = &fakeBox{image: image, running: running}
}

func (b *fakeBackend) history() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

func (b *fakeBackend) count(call string) int {
	n := 0
	for _, c := range b.history() {
		if c == call || strings.HasPrefix(c, call+":") {
			n++
		}
	}
	return n
}

// alive reports whether the named sandbox exists on the backend and whether its
// compute is running, which is how a test tells "stopped" from "destroyed".
func (b *fakeBackend) alive(name string) (exists, running bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	box, ok := b.boxes[name]
	return ok, ok && box.running
}

func (b *fakeBackend) observed(call string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sawExec[call]
}

func (b *fakeBackend) setStopErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopErr = err
}

// fakeClock is the manager's time under test control. Advance runs due timers
// on the calling goroutine, so a test asserts the result immediately instead of
// polling for it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	fn      func()
	fired   bool
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
}

var _ Clock = (*fakeClock)(nil)

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves time forward and fires every timer that comes due, earliest
// first. A callback that arms a new timer is handled: the new one only fires if
// it too is already due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()

	for t := c.due(); t != nil; t = c.due() {
		t.fn()
	}
}

func (c *fakeClock) due() *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var next *fakeTimer
	for _, t := range c.timers {
		if t.fired || t.stopped || t.at.After(c.now) {
			continue
		}
		if next == nil || t.at.Before(next.at) {
			next = t
		}
	}
	if next != nil {
		next.fired = true
	}
	return next
}
