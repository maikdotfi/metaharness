package server

import (
	"context"
	"errors"
	"os/exec"
)

// Machine is the sandbox an agent's tools run inside. The central architectural
// bet of metaharness is that tools (which live in the server process) reach
// *into* the machine to do work — the harness is never trapped inside the
// sandbox. This interface is that seam: every tool that touches the outside
// world does so through a Machine, so swapping LocalMachine for an Incus
// system container (or anything else) later changes nothing above this line.
type Machine interface {
	// Exec runs a shell command inside the machine and returns its combined
	// output, exit code, and any error launching the process.
	Exec(ctx context.Context, command string) (output string, exitCode int, err error)
}

// LocalMachine cheats: it runs commands directly on the host via `sh -c`,
// rooted at a workspace directory. No isolation whatsoever — it exists purely
// to prove the Machine seam end-to-end before we wire a real sandbox.
type LocalMachine struct {
	Workdir string
}

func (m *LocalMachine) Exec(ctx context.Context, command string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = m.Workdir
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		// A non-zero exit is a normal, expected result the agent should see,
		// not a transport error — surface the code, keep err nil.
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return string(out), exitErr.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), exitCode, nil
}
