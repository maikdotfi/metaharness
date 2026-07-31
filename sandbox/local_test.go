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

// mustDir is the whole assembly for a caller that wants one directory and no
// manager, so the tests below go through exactly the call an application makes.
func mustDir(t *testing.T, path string) agent.Sandbox {
	t.Helper()
	box, err := Dir(path)
	if err != nil {
		t.Fatalf("Dir(%q): %v", path, err)
	}
	t.Cleanup(func() { box.Close() })
	return box
}

// TestDirIsTheWholeAssembly is the shape a single-sandbox application is meant
// to have: one call turns a path into something a session can be given, with no
// backend kind, no root-and-name pair and nothing to close but the handle.
func TestDirIsTheWholeAssembly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	box := mustDir(t, dir)

	sess := agent.NewSession("task-1", "some-model", box)
	if sess.Sandbox() == nil {
		t.Fatal("the session did not take the sandbox")
	}
	if sess.SandboxName() != "work" {
		t.Errorf("SandboxName() = %q, want %q", sess.SandboxName(), "work")
	}
}

// TestDirCreatesThePath checks a path that is not there yet is created, so the
// promise is the same one Open makes: asking for a sandbox is enough to have it.
func TestDirCreatesThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "work")
	box := mustDir(t, dir)

	if _, err := box.Exec(context.Background(), bash("echo hi > out.txt")); err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("expected the command to have run in a created %s: %v", dir, err)
	}
}

// TestDirNameIsTheDirectoryName checks the name a session records is the
// directory's own, whatever syntax the path was written in — it is what a
// restored session would look its sandbox up by.
func TestDirNameIsTheDirectoryName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	for _, path := range []string{dir, dir + "/.", dir + "/sub/.."} {
		if got := mustDir(t, path).Name(); got != "work" {
			t.Errorf("Dir(%q).Name() = %q, want %q", path, got, "work")
		}
	}
}

// TestDirRefusesNoPath checks the empty path is an error rather than the process
// working directory: an agent with shell tools loose in someone's source tree is
// not a default worth having.
func TestDirRefusesNoPath(t *testing.T) {
	if _, err := Dir(""); err == nil {
		t.Fatal("expected an error for an empty path, got nil")
	}
}

// TestDirRefusesAFile checks a path that exists as something other than a
// directory fails at the call rather than at the first command.
func TestDirRefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Dir(path); err == nil {
		t.Fatal("expected an error for a path that is a file, got nil")
	}
}

// TestLocalExecRunsInDir proves commands run with their working directory set
// to the sandbox's path: a relative write lands inside it, not in the process
// cwd. Checking the file's location sidesteps the symlinked-temp-dir pitfall
// that comparing `pwd` output would hit (e.g. /var vs /private/var on macOS).
func TestLocalExecRunsInDir(t *testing.T) {
	dir := t.TempDir()
	box := mustDir(t, dir)

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
	box := mustDir(t, t.TempDir())
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
	box := mustDir(t, t.TempDir())
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
	box := mustDir(t, t.TempDir())
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
	box := mustDir(t, t.TempDir())
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
		if err := backend.EnsureReady(ctx, Spec{Name: name}); err != nil {
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
	spec := Spec{Name: "work"}

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
	if err := backend.EnsureReady(ctx, Spec{Name: "work"}); err != nil {
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
		if err := backend.EnsureReady(ctx, Spec{Name: name}); err != nil {
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
			if err := backend.EnsureReady(context.Background(), Spec{Name: name}); err == nil {
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

	clock := newFakeClock()
	first, err := New(LocalKind, WithRoot(root), WithClock(clock), WithIdleTimeout(testIdle))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	box, err := first.Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := box.Exec(ctx, bash("echo note > kept.txt")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	box.Close()
	clock.Advance(testIdle)
	first.Close()

	// A new process over the same sandboxes, assembled the way an application
	// assembles one: a kind and where they live. It is told nothing about what
	// survived, and works in the same sandbox anyway.
	second, err := New(LocalKind, WithRoot(root), WithClock(newFakeClock()), WithIdleTimeout(testIdle))
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	defer second.Close()

	resumed, err := second.Open("work")
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
