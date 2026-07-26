// Package testutils provides fakes and helpers shared across the project's
// tests: sandbox doubles, a scripted fake model, an in-memory store, and
// helpers for driving tools and the agent loop.
package testutils

import (
	"context"
	"os/exec"
	"slices"
	"sync"

	"github.com/maikdotfi/metaharness/agent"
)

// RealSandbox runs commands for real via os/exec, so a test exercises the
// actual shell rather than a mock. Tool tests use this to stress the real
// cat/base64 plumbing. The tools invoke e.g. "bash -c <cmd>".
type RealSandbox struct{}

func (RealSandbox) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd.Cmd, cmd.Args...)
	out, err := c.Output()
	res := agent.ExecResult{Stdout: string(out), ExitCode: c.ProcessState.ExitCode()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Stderr = string(ee.Stderr)
	}
	return res, nil
}

func (RealSandbox) Close() error { return nil }

// NopSandbox is a do-nothing Sandbox for loop tests whose tools never exec.
type NopSandbox struct{}

func (NopSandbox) Exec(context.Context, agent.Command) (agent.ExecResult, error) {
	return agent.ExecResult{}, nil
}

func (NopSandbox) Close() error { return nil }

// NopFactory hands out NopSandbox handles, satisfying the SandboxFactory the
// agent loop acquires from. It implements neither Sleeper, Destroyer, nor
// Lister, which makes it the double for "a backend without those capabilities".
type NopFactory struct{}

func (NopFactory) Acquire(context.Context, agent.SandboxSpec) (agent.Sandbox, error) {
	return NopSandbox{}, nil
}

// SleepySandbox is a Sandbox that also implements agent.Sleeper. It records an
// ordered log of the calls it received — "exec", "sleep", "wake", "close" — so a
// test can assert an exact sequence such as exec, exec, sleep, wake, exec.
type SleepySandbox struct {
	// Result is what every Exec returns.
	Result agent.ExecResult

	// OnSleep and OnWake, when set, decide each call's outcome. They are handed
	// the 1-based call number, so a test can fail the first Sleep and let the
	// retry succeed.
	OnSleep func(n int) error
	OnWake  func(n int) error

	// Gate, when non-nil, holds every Exec until it is closed or hands over a
	// value, standing in for a command that takes a long time.
	Gate chan struct{}

	// Entered, when non-nil, receives once per Exec just before that Exec waits
	// on Gate. It lets a test know a command is genuinely in flight without
	// polling or sleeping.
	Entered chan struct{}

	mu       sync.Mutex
	log      []string
	commands []agent.Command
	sleeps   int
	wakes    int
}

var (
	_ agent.Sandbox = (*SleepySandbox)(nil)
	_ agent.Sleeper = (*SleepySandbox)(nil)
)

func (s *SleepySandbox) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	s.mu.Lock()
	s.log = append(s.log, "exec")
	s.commands = append(s.commands, cmd)
	gate, entered, res := s.Gate, s.Entered, s.Result
	s.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return agent.ExecResult{}, ctx.Err()
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return agent.ExecResult{}, ctx.Err()
		}
	}
	return res, nil
}

func (s *SleepySandbox) Sleep(context.Context) error {
	s.mu.Lock()
	s.log = append(s.log, "sleep")
	s.sleeps++
	n, hook := s.sleeps, s.OnSleep
	s.mu.Unlock()
	if hook != nil {
		return hook(n)
	}
	return nil
}

func (s *SleepySandbox) Wake(context.Context) error {
	s.mu.Lock()
	s.log = append(s.log, "wake")
	s.wakes++
	n, hook := s.wakes, s.OnWake
	s.mu.Unlock()
	if hook != nil {
		return hook(n)
	}
	return nil
}

func (s *SleepySandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, "close")
	return nil
}

// Log returns the calls this sandbox received, in order.
func (s *SleepySandbox) Log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

// Commands returns the commands passed to Exec, in order.
func (s *SleepySandbox) Commands() []agent.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Count returns how many times one kind of call was made.
func (s *SleepySandbox) Count(kind string) int {
	n := 0
	for _, entry := range s.Log() {
		if entry == kind {
			n++
		}
	}
	return n
}

// SleepyFactory hands out SleepySandbox handles and records every spec it was
// asked for, so a test can prove how often something reached the backend. It
// also implements agent.Destroyer and agent.Lister, standing in for a backend
// that can be reconciled against.
type SleepyFactory struct {
	// OnSleep, OnWake, Gate and Entered are copied into every sandbox handed out.
	OnSleep func(n int) error
	OnWake  func(n int) error
	Gate    chan struct{}
	Entered chan struct{}

	// Err, when set, is the error Acquire fails with.
	Err error

	// Listing is the backend ground truth List reports.
	Listing []agent.SandboxInfo

	mu        sync.Mutex
	specs     []agent.SandboxSpec
	boxes     []*SleepySandbox
	destroyed []string
}

var (
	_ agent.SandboxFactory = (*SleepyFactory)(nil)
	_ agent.Destroyer      = (*SleepyFactory)(nil)
	_ agent.Lister         = (*SleepyFactory)(nil)
)

func (f *SleepyFactory) Acquire(_ context.Context, spec agent.SandboxSpec) (agent.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.Err != nil {
		return nil, f.Err
	}
	box := &SleepySandbox{OnSleep: f.OnSleep, OnWake: f.OnWake, Gate: f.Gate, Entered: f.Entered}
	f.boxes = append(f.boxes, box)
	return box, nil
}

func (f *SleepyFactory) Destroy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, name)
	return nil
}

func (f *SleepyFactory) List(context.Context) ([]agent.SandboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Listing), nil
}

// Specs returns the specs Acquire was called with, in order.
func (f *SleepyFactory) Specs() []agent.SandboxSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// Boxes returns the sandboxes handed out, in order.
func (f *SleepyFactory) Boxes() []*SleepySandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.boxes)
}

// Destroyed returns the names Destroy was called with, in order.
func (f *SleepyFactory) Destroyed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.destroyed)
}
