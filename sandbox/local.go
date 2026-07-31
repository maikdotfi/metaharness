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

// Dir is a sandbox that is one directory on this machine, for an application
// that works in a single place and has no use for a Manager: there is no backend
// kind to choose, no idle policy, and the handle is the only thing to close.
//
//	box, err := sandbox.Dir("workspace")
//	sess := agent.NewSession(id, modelID, box)
//
// The directory is created if it is not there yet, which is the promise
// Manager.Open makes too — asking for a sandbox is enough to have it. The path
// is in the signature because there is no path worth defaulting to: an agent
// with shell tools loose in whatever directory the process started from is a
// surprise, not a convenience.
//
// The name a session records is the directory's own — Dir("/srv/work") is the
// sandbox "work" — which is the same name the local backend would give it, so a
// session written by one is one a Manager can resume.
//
// It is NOT a sandbox in any security sense. There is no isolation, no resource
// limits, and nothing stopping a command from reaching outside the directory: an
// absolute path or a `cd ..` escapes it trivially. It sets where commands start
// and nothing more. Point it at a scratch directory, not at a machine you would
// mind an agent breaking; for isolation, take a Manager over a backend that has
// some.
func Dir(path string) (agent.Sandbox, error) {
	if path == "" {
		return nil, errors.New("sandbox: Dir needs a directory to work in")
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	return &local{dir: path}, nil
}

// local runs commands directly on the host machine, with each process's working
// directory set to dir. It is what Dir hands out and what LocalBackend runs
// commands through, so both reach the host the same way.
type local struct {
	// dir is the working directory commands run in. An empty dir means the
	// current process working directory, which only the backend's own use can
	// produce — Dir refuses it.
	dir string
}

var _ agent.Sandbox = (*local)(nil)

// Name is the directory's own name, which is exactly what LocalBackend uses as a
// sandbox name: the sandbox "work" under Root is the directory Root/work.
func (l *local) Name() string {
	if l.dir == "" {
		return "local"
	}
	return filepath.Base(l.dir)
}

// Exec runs cmd on the host, capturing stdout and stderr. A command that runs
// to completion — even with a non-zero exit status — returns a populated
// ExecResult and a nil error: the exit code is a normal outcome the caller
// inspects, not a failure of the sandbox. A nil error with a non-zero ExitCode
// is the "the command failed" case. A non-nil error is reserved for the command
// never running at all (binary not found, directory gone, context cancelled),
// which the agent loop treats as fatal infra failure.
func (l *local) Exec(ctx context.Context, cmd agent.Command) (agent.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd.Cmd, cmd.Args...)
	c.Dir = l.dir

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

// Close releases the handle and nothing else: the directory and everything in it
// survive it, as they must — a session ending is not a reason to lose the work.
// There is no resource to release either way, but it keeps callers' defer
// box.Close() uniform across implementations.
func (l *local) Close() error { return nil }

// LocalBackend gives each named sandbox its own directory under Root and runs
// its commands there on the host. It is the development backend behind a
// Manager: the filesystem persists because it is only a directory, and the
// image in a spec is ignored because there is nothing to pull.
//
// It inherits Dir's lack of isolation, and adds no compute of its own, so
// there is nothing to release: Stop does nothing and List reports every sandbox
// as stopped.
type LocalBackend struct {
	// Root is the directory sandbox directories are created under. It is
	// created, with parents, on first use.
	Root string
}

var _ Backend = LocalBackend{}

// LocalKind is the name LocalBackend answers to in New, and what an empty kind
// means there. Unlike the other backends it needs no import to be available: it
// lives in this package, brings no dependencies, and being what an application
// falls back to is the point of it.
const LocalKind = "local"

func init() {
	Register(LocalKind, func(cfg Config) (Backend, error) {
		// There is no sensible default here. An empty Root would put sandboxes in
		// whatever directory the process was started from, which is someone's
		// source tree often enough that a refusal is kinder than a surprise.
		if cfg.Root == "" {
			return nil, errors.New("sandbox: the local backend needs a Root to create sandboxes under")
		}
		return LocalBackend{Root: cfg.Root}, nil
	})
}

func (b LocalBackend) EnsureReady(_ context.Context, spec Spec) error {
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
	return (&local{dir: dir}).Exec(ctx, cmd)
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

// Close does nothing: the host filesystem is not a connection this backend
// holds open. The sandbox directories stay where they are.
func (b LocalBackend) Close() error { return nil }

// dir keeps a sandbox name a name. Anything that could point somewhere other
// than a direct child of Root — a path separator, a parent reference, an
// absolute path — is rejected rather than resolved.
func (b LocalBackend) dir(name string) (string, error) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return "", fmt.Errorf("sandbox: %q is not a usable local sandbox name", name)
	}
	return filepath.Join(b.Root, name), nil
}
