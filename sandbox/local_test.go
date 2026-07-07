package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

func bash(cmd string) agent.Command {
	return agent.Command{Cmd: "bash", Args: []string{"-c", cmd}}
}

// TestLocalExecRunsInDir proves commands run with their working directory set
// to Dir: a relative write lands inside Dir, not the process cwd. Checking the
// file's location sidesteps the symlinked-temp-dir pitfall that comparing
// `pwd` output would hit (e.g. /var vs /private/var on macOS).
func TestLocalExecRunsInDir(t *testing.T) {
	dir := t.TempDir()
	box := &Local{Dir: dir}

	res, err := box.Exec(context.Background(), bash("echo hi > out.txt"))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr: %q)", res.ExitCode, res.Stderr)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("expected out.txt inside Dir: %v", err)
	}
	if string(got) != "hi\n" {
		t.Errorf("out.txt = %q, want %q", got, "hi\n")
	}
}

func TestLocalExecStdout(t *testing.T) {
	box := &Local{Dir: t.TempDir()}
	res, err := box.Exec(context.Background(), bash("echo hello"))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello\n")
	}
}

// TestLocalExecExitCode checks a non-zero exit is reported through ExitCode with
// a nil error — the "command failed" case, not an infra failure.
func TestLocalExecExitCode(t *testing.T) {
	box := &Local{Dir: t.TempDir()}
	res, err := box.Exec(context.Background(), bash("exit 3"))
	if err != nil {
		t.Fatalf("non-zero exit should not be an infra error, got: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// TestLocalExecStderr checks stderr is captured even on failure.
func TestLocalExecStderr(t *testing.T) {
	box := &Local{Dir: t.TempDir()}
	res, err := box.Exec(context.Background(), bash("echo boom >&2; exit 1"))
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	if res.Stderr != "boom\n" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "boom\n")
	}
}

// TestLocalExecInfraError checks that a command that never runs (binary not
// found) surfaces as a non-nil error, which the agent loop treats as fatal.
func TestLocalExecInfraError(t *testing.T) {
	box := &Local{Dir: t.TempDir()}
	_, err := box.Exec(context.Background(), agent.Command{Cmd: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Fatal("expected an infra error for a missing binary, got nil")
	}
}

// TestLocalFactoryAcquireCreatesRoot checks the factory creates a missing root
// directory and hands back a sandbox rooted there.
func TestLocalFactoryAcquireCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "work")
	f := LocalFactory{Root: root}

	box, err := f.Acquire(context.Background(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer box.Close()

	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("Acquire should have created %s as a directory (err: %v)", root, err)
	}
	if local, ok := box.(*Local); !ok || local.Dir != root {
		t.Errorf("acquired sandbox = %#v, want *Local rooted at %s", box, root)
	}
}
