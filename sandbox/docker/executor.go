package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/maikdotfi/metaharness/agent"
)

// Exec runs one command inside the running sandbox and waits for it to finish.
//
// A command that ran and exited non-zero is an ExecResult carrying that code and
// a nil error: what the command made of its arguments is the caller's business,
// not a failure of the sandbox. An error means the command never ran, or that its
// outcome could not be established — the cases the agent loop treats as
// infrastructure failure.
func (b *Backend) Exec(ctx context.Context, name string, cmd agent.Command) (agent.ExecResult, error) {
	cname, err := b.containerName(name)
	if err != nil {
		return agent.ExecResult{}, err
	}

	created, err := b.daemon.ContainerExecCreate(ctx, cname, container.ExecOptions{
		Cmd:          append([]string{cmd.Cmd}, cmd.Args...),
		WorkingDir:   b.workdir,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return agent.ExecResult{}, fmt.Errorf("sandbox/docker: starting a command in sandbox %q: %w", name, err)
	}

	stdout, stderr, err := b.stream(ctx, created.ID)
	if err != nil {
		return agent.ExecResult{}, fmt.Errorf("sandbox/docker: running a command in sandbox %q: %w", name, err)
	}

	// The exit status is only final once the output has ended, so it is read
	// after the stream has been drained rather than alongside it.
	finished, err := b.daemon.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return agent.ExecResult{}, fmt.Errorf("sandbox/docker: reading the outcome of a command in sandbox %q: %w", name, err)
	}
	if finished.Running {
		return agent.ExecResult{}, fmt.Errorf("sandbox/docker: no exit status for a command in sandbox %q: its output ended but the daemon still reports it running", name)
	}

	return agent.ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: finished.ExitCode}, nil
}

// stream attaches to a created exec and reads it to the end, separating the two
// output streams the daemon interleaves down one connection.
//
// Cancellation works by closing that connection under the read. A read blocked
// on the daemon does not notice a context, so closing it is the only thing that
// makes an interrupted command return promptly.
func (b *Backend) stream(ctx context.Context, execID string) (stdout, stderr string, err error) {
	attached, err := b.daemon.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
	if err != nil {
		return "", "", err
	}
	if attached.Conn == nil || attached.Reader == nil {
		return "", "", errors.New("the daemon attached no output stream to the command")
	}
	defer attached.Close()

	var out, errOut bytes.Buffer
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&out, &errOut, attached.Reader)
		copied <- err
	}()

	select {
	case err := <-copied:
		if err != nil {
			return "", "", err
		}
	case <-ctx.Done():
		// Closing the connection is what unblocks the read; waiting for the copy
		// to return afterwards is what makes the buffers nobody else's business.
		attached.Close()
		<-copied
		return "", "", ctx.Err()
	}
	return out.String(), errOut.String(), nil
}
