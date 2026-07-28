// Package testutils provides fakes and helpers shared across the project's
// tests: sandbox doubles, a scripted fake model, an in-memory store, and
// helpers for driving tools and the agent loop.
package testutils

import (
	"context"
	"os/exec"
	"sync"

	"github.com/maikdotfi/metaharness/agent"
)

// RealSandbox runs commands for real via os/exec, so a test exercises the
// actual shell rather than a mock. Tool tests use this to stress the real
// cat/base64 plumbing. The tools invoke e.g. "bash -c <cmd>".
type RealSandbox struct{}

// Name identifies the sandbox the way any other handle does. Nothing here is
// named or shared, so one constant is enough.
func (RealSandbox) Name() string { return "real" }

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

// FakeSandbox is a named sandbox that records what it was asked to run and runs
// none of it. Loop tests use it to tell which sandbox a session's tools reached:
// every command comes back with the sandbox's own name as stdout.
type FakeSandbox struct {
	SandboxName string

	mu       sync.Mutex
	commands []agent.Command
	closes   int
}

var _ agent.Sandbox = (*FakeSandbox)(nil)

func (f *FakeSandbox) Name() string { return f.SandboxName }

func (f *FakeSandbox) Exec(_ context.Context, cmd agent.Command) (agent.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	return agent.ExecResult{Stdout: f.SandboxName}, nil
}

func (f *FakeSandbox) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// Commands returns the commands this sandbox was asked to run, in order.
func (f *FakeSandbox) Commands() []agent.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Command(nil), f.commands...)
}

// Closes reports how many times the handle was closed, so a test can pin who
// closes a sandbox and when.
func (f *FakeSandbox) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}
