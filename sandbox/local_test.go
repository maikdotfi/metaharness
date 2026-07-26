package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// TestLocalBackendGivesEachNameItsOwnDirectory checks the name is what
// separates two sandboxes on this backend.
func TestLocalBackendGivesEachNameItsOwnDirectory(t *testing.T) {
	backend := LocalBackend{Root: filepath.Join(t.TempDir(), "nested")}
	ctx := context.Background()

	for _, name := range []string{"one", "two"} {
		if err := backend.EnsureReady(ctx, agent.SandboxSpec{Name: name}); err != nil {
			t.Fatalf("EnsureReady(%q): %v", name, err)
		}
		if _, err := backend.Exec(ctx, name, bash("echo "+name+" > who.txt")); err != nil {
			t.Fatalf("Exec(%q): %v", name, err)
		}
	}

	for _, name := range []string{"one", "two"} {
		got, err := os.ReadFile(filepath.Join(backend.Root, name, "who.txt"))
		if err != nil {
			t.Fatalf("sandbox %q should have its own directory: %v", name, err)
		}
		if string(got) != name+"\n" {
			t.Errorf("sandbox %q holds %q, want %q", name, got, name+"\n")
		}
	}
}

// TestLocalBackendFilesystemSurvivesStop is the persistence promise: releasing a
// sandbox's compute must not cost it any work.
func TestLocalBackendFilesystemSurvivesStop(t *testing.T) {
	backend := LocalBackend{Root: t.TempDir()}
	ctx := context.Background()
	spec := agent.SandboxSpec{Name: "work"}

	if err := backend.EnsureReady(ctx, spec); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if _, err := backend.Exec(ctx, "work", bash("echo saved > note.txt")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := backend.Stop(ctx, "work"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := backend.EnsureReady(ctx, spec); err != nil {
		t.Fatalf("EnsureReady after Stop: %v", err)
	}

	res, err := backend.Exec(ctx, "work", bash("cat note.txt"))
	if err != nil {
		t.Fatalf("Exec after Stop: %v", err)
	}
	if res.Stdout != "saved\n" {
		t.Errorf("after stopping and waking, note.txt = %q, want %q", res.Stdout, "saved\n")
	}
}

// TestLocalBackendDestroyRemovesFilesystem checks destruction is the one thing
// that does take the work with it, and that repeating it is fine.
func TestLocalBackendDestroyRemovesFilesystem(t *testing.T) {
	backend := LocalBackend{Root: t.TempDir()}
	ctx := context.Background()
	if err := backend.EnsureReady(ctx, agent.SandboxSpec{Name: "work"}); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if err := backend.Destroy(ctx, "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := backend.Destroy(ctx, "work"); err != nil {
		t.Fatalf("destroying an absent sandbox should be success, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backend.Root, "work")); !os.IsNotExist(err) {
		t.Errorf("Destroy left the sandbox directory behind (err: %v)", err)
	}
}

// TestLocalBackendListsWhatSurvived checks a restarted process can find the
// sandboxes left on disk.
func TestLocalBackendListsWhatSurvived(t *testing.T) {
	backend := LocalBackend{Root: t.TempDir()}
	ctx := context.Background()
	for _, name := range []string{"alpha", "beta"} {
		if err := backend.EnsureReady(ctx, agent.SandboxSpec{Name: name}); err != nil {
			t.Fatalf("EnsureReady(%q): %v", name, err)
		}
	}

	found, err := backend.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, box := range found {
		names = append(names, box.Name)
		if box.State != BackendStopped {
			t.Errorf("%q reported as %v; a directory holds no compute", box.Name, box.State)
		}
	}
	if want := []string{"alpha", "beta"}; !slices.Equal(names, want) {
		t.Errorf("List() = %v, want %v", names, want)
	}
}

// TestLocalBackendListsNothingBeforeFirstUse checks a root that does not exist
// yet is an empty backend, not a failure to start up.
func TestLocalBackendListsNothingBeforeFirstUse(t *testing.T) {
	backend := LocalBackend{Root: filepath.Join(t.TempDir(), "missing")}

	found, err := backend.List(context.Background())
	if err != nil {
		t.Fatalf("List on an unused root: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("List() = %v, want nothing", found)
	}
}

// TestLocalBackendRejectsNamesThatEscapeRoot checks a sandbox name stays a name:
// it can never reach outside Root.
func TestLocalBackendRejectsNamesThatEscapeRoot(t *testing.T) {
	backend := LocalBackend{Root: t.TempDir()}

	for _, name := range []string{"", ".", "..", "../escape", "nested/work", "/abs"} {
		t.Run(name, func(t *testing.T) {
			if err := backend.EnsureReady(context.Background(), agent.SandboxSpec{Name: name}); err == nil {
				t.Errorf("EnsureReady(%q) should be rejected", name)
			}
		})
	}
}

// TestManagedSandboxOutlivesTheProcess is the whole point of the design, over a
// real backend rather than a fake: work done in a named sandbox is still there
// after the sandbox goes idle and the process that used it is gone, and stays
// there until someone destroys it.
func TestManagedSandboxOutlivesTheProcess(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	spec := agent.SandboxSpec{Name: "work"}

	clock := newFakeClock()
	first := NewManager(LocalBackend{Root: root}, WithClock(clock), WithIdleTimeout(testIdle))
	box, err := first.Open(spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := box.Exec(ctx, bash("echo note > kept.txt")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	box.Close()
	clock.Advance(testIdle)

	// A new process over the same backend: it learns what survived, then works in
	// the same sandbox.
	second := NewManager(LocalBackend{Root: root}, WithClock(newFakeClock()), WithIdleTimeout(testIdle))
	report, err := second.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Contains(report.Asleep, "work") {
		t.Errorf("Reconcile did not find the sandbox: %+v", report)
	}

	resumed, err := second.Open(spec)
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	res, err := resumed.Exec(ctx, bash("cat kept.txt"))
	if err != nil {
		t.Fatalf("Exec after restart: %v", err)
	}
	if res.Stdout != "note\n" {
		t.Errorf("kept.txt = %q, want %q: the sandbox did not survive", res.Stdout, "note\n")
	}

	if err := second.Destroy(ctx, "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work")); !os.IsNotExist(err) {
		t.Errorf("Destroy should be the one thing that removes it (err: %v)", err)
	}
}
