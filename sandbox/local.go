// Package sandbox provides concrete agent.Sandbox implementations.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

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
// do — but it satisfies the Sandbox interface and keeps callers' defer box.Close()
// uniform across implementations.
func (l *Local) Close() error { return nil }

// LocalBackend gives each named sandbox its own directory under Root and runs
// its commands there on the host. It is the development backend behind a
// Manager: the filesystem persists because it is only a directory, and the
// image in a spec is ignored because there is nothing to pull.
//
// It inherits Local's lack of isolation, and adds no compute of its own, so
// there is nothing to release: Stop does nothing and List reports every sandbox
// as stopped.
type LocalBackend struct {
	// Root is the directory sandbox directories are created under. It is
	// created, with parents, on first use.
	Root string
}

var _ Backend = LocalBackend{}

func (b LocalBackend) EnsureReady(_ context.Context, spec agent.SandboxSpec) error {
	dir, err := b.dir(spec.Name)
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

func (b LocalBackend) Exec(ctx context.Context, name string, cmd agent.Command) (agent.ExecResult, error) {
	dir, err := b.dir(name)
	if err != nil {
		return agent.ExecResult{}, err
	}
	return (&Local{Dir: dir}).Exec(ctx, cmd)
}

// Stop does nothing: a directory holds no compute to release.
func (b LocalBackend) Stop(context.Context, string) error { return nil }

// Destroy removes the sandbox's directory. A sandbox that is already gone is
// success.
func (b LocalBackend) Destroy(_ context.Context, name string) error {
	dir, err := b.dir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// List reports the directories under Root as stopped sandboxes. A Root that
// does not exist yet is simply an empty backend.
func (b LocalBackend) List(context.Context) ([]BackendSandbox, error) {
	entries, err := os.ReadDir(b.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []BackendSandbox
	for _, entry := range entries {
		if entry.IsDir() {
			found = append(found, BackendSandbox{Name: entry.Name(), State: BackendStopped})
		}
	}
	return found, nil
}

// dir keeps a sandbox name a name. Anything that could point somewhere other
// than a direct child of Root — a path separator, a parent reference, an
// absolute path — is rejected rather than resolved.
func (b LocalBackend) dir(name string) (string, error) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return "", fmt.Errorf("sandbox: %q is not a usable local sandbox name", name)
	}
	return filepath.Join(b.Root, name), nil
}
