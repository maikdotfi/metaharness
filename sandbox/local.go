// Package sandbox provides concrete agent.Sandbox implementations.
package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"

	"github.com/maikdotfi/metaharness/agent"
)

// Local runs commands directly on the host machine, with each process's working
// directory set to Dir.
//
// It is NOT a sandbox in any security sense. There is no isolation, no resource
// limits, and nothing stopping a command from reaching outside Dir — an
// absolute path or a `cd ..` escapes it trivially. Dir only sets where commands
// start. Local exists for local development and tests, where running against
// real files in a scratch directory is worth more than isolation. Do not point
// it at a machine you would mind an agent breaking.
type Local struct {
	// Dir is the working directory commands run in. An empty Dir means the
	// current process working directory.
	Dir string
}

var _ agent.Sandbox = (*Local)(nil)

// Exec runs cmd on the host, capturing stdout and stderr. A command that runs
// to completion — even with a non-zero exit status — returns a populated
// ExecResult and a nil error: the exit code is a normal outcome the caller
// inspects, not a failure of the sandbox. A nil error with a non-zero ExitCode
// is the "the command failed" case. A non-nil error is reserved for the command
// never running at all (binary not found, Dir missing, context cancelled),
// which the agent loop treats as fatal infra failure.
func (l *Local) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd.Cmd, cmd.Args...)
	c.Dir = l.Dir

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	if err != nil {
		// The process ran and exited non-zero: a normal result, reported
		// through ExitCode rather than the error.
		if _, ok := err.(*exec.ExitError); !ok {
			return agent.ExecResult{}, err
		}
	}
	return agent.ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: c.ProcessState.ExitCode(),
	}, nil
}

// Close releases the handle. Local holds no resources, so there is nothing to
// do — which is already the detach the Sandbox interface asks for: closing a
// handle never takes a sandbox away. It keeps callers' defer box.Close() uniform
// across implementations.
//
// Local does not implement agent.Sleeper: there is no compute to release when the
// commands are host processes. A registry wrapping it therefore never arms a
// sleep deadline.
func (l *Local) Close() error { return nil }

// LocalFactory hands out Local sandboxes, all rooted at Root. It implements
// agent.SandboxFactory so it can be passed to agent.WithSandbox. The
// SandboxSpec is ignored: there are no images to pull locally.
type LocalFactory struct {
	// Root is the working directory every acquired sandbox runs in. It is
	// created (including parents) on Acquire if it does not already exist.
	Root string
}

var _ agent.SandboxFactory = LocalFactory{}

// Acquire returns a Local rooted at f.Root, creating that directory if needed.
func (f LocalFactory) Acquire(ctx context.Context, _ agent.SandboxSpec) (agent.Sandbox, error) {
	if f.Root != "" {
		if err := os.MkdirAll(f.Root, 0o755); err != nil {
			return nil, err
		}
	}
	return &Local{Dir: f.Root}, nil
}
