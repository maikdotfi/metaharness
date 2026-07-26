// Package testutils provides fakes and helpers shared across the project's
// tests: sandbox doubles, a scripted fake model, an in-memory store, and
// helpers for driving tools and the agent loop.
package testutils

import (
	"context"
	"os/exec"

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
// agent loop acquires from.
type NopFactory struct{}

func (NopFactory) Open(agent.SandboxSpec) (agent.Sandbox, error) {
	return NopSandbox{}, nil
}
