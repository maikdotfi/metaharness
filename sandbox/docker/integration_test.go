//go:build docker

// The tests in this file need a real Docker daemon and are built only under the
// `docker` tag:
//
//	go test -race -tags=docker ./sandbox/docker
//
// They are the counterpart to the fake-daemon suite: that one pins every branch
// of the decision table, this one checks the branches mean on a real daemon what
// they were written to mean.
package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/sandbox"
)

const (
	// liveImage is small, common, and busybox-based, which is the case the
	// default keepalive has to survive.
	liveImage = "alpine:3.21"
	liveName  = "integration"
)

func liveSpec() agent.SandboxSpec {
	return agent.SandboxSpec{Name: liveName, Image: liveImage}
}

// liveBackend connects to the daemon, or skips if there is none. It leaves no
// sandbox behind, and starts from nothing even if an earlier run died holding
// one.
func liveBackend(t *testing.T) *Backend {
	t.Helper()

	b, err := New()
	if err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	if _, err := b.List(context.Background()); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Destroy(context.Background(), liveName); err != nil {
			t.Errorf("cleaning up: %v", err)
		}
		b.Close()
	})
	if err := b.Destroy(context.Background(), liveName); err != nil {
		t.Fatalf("clearing an earlier run: %v", err)
	}
	return b
}

func sh(script string) agent.Command {
	return agent.Command{Cmd: "sh", Args: []string{"-c", script}}
}

// TestLiveSandboxOutlivesItsCompute is the contract end to end on a real daemon:
// a sandbox is created on demand, runs commands, releases its compute, wakes
// with the work still there, and is only emptied by destruction.
func TestLiveSandboxOutlivesItsCompute(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()

	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if _, err := b.Exec(ctx, liveName, sh("echo kept > note.txt")); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if err := b.Stop(ctx, liveName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	found, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if state, ok := live(found, liveName); !ok || state != sandbox.BackendStopped {
		t.Errorf("after Stop the sandbox is %v (present: %v), want stopped", state, ok)
	}

	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady after Stop: %v", err)
	}
	res, err := b.Exec(ctx, liveName, sh("cat note.txt"))
	if err != nil {
		t.Fatalf("Exec after waking: %v", err)
	}
	if res.Stdout != "kept\n" {
		t.Errorf("note.txt = %q, want %q: the sandbox did not survive losing its compute", res.Stdout, "kept\n")
	}

	if err := b.Destroy(ctx, liveName); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	found, err = b.List(ctx)
	if err != nil {
		t.Fatalf("List after Destroy: %v", err)
	}
	if _, ok := live(found, liveName); ok {
		t.Error("Destroy left the sandbox behind")
	}
	if err := b.Destroy(ctx, liveName); err != nil {
		t.Errorf("destroying an absent sandbox should be success, got: %v", err)
	}
}

// TestLiveExecContract checks the command contract on a real daemon: both
// streams preserved, the guest's exit code reported as a result rather than an
// error, and commands running in the sandbox's working directory.
func TestLiveExecContract(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()
	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	res, err := b.Exec(ctx, liveName, sh("echo out; echo err >&2; exit 3"))
	if err != nil {
		t.Fatalf("a failing command is not an infra error, got: %v", err)
	}
	if res.Stdout != "out\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "err\n")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}

	if res, err = b.Exec(ctx, liveName, sh("pwd")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != DefaultWorkdir {
		t.Errorf("commands run in %q, want %q", got, DefaultWorkdir)
	}

	// Output well past one read, to check nothing is lost in the framing.
	if res, err = b.Exec(ctx, liveName, sh("seq 1 200000")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if lines := strings.Count(res.Stdout, "\n"); lines != 200000 {
		t.Errorf("got %d lines of output, want 200000", lines)
	}
}

// TestLiveExecCancellationReturnsPromptly checks a cancelled command does not
// leave the caller waiting on a stream the daemon is in no hurry to close.
func TestLiveExecCancellationReturnsPromptly(t *testing.T) {
	b := liveBackend(t)
	if err := b.EnsureReady(context.Background(), liveSpec()); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := b.Exec(ctx, liveName, sh("sleep 60"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Exec = %v, want it to report the deadline", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("Exec took %v to give up on a cancelled command", waited)
	}
}

// TestLiveDestroyDoesNotWaitForTheKeepalive checks removing a running sandbox is
// prompt. The keepalive cannot be signalled, so a daemon asked to stop it
// gracefully waits out its full timeout — ten seconds, by default, on every
// destroy.
func TestLiveDestroyDoesNotWaitForTheKeepalive(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()
	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	start := time.Now()
	if err := b.Destroy(ctx, liveName); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("Destroy took %v, which means it waited for a graceful stop", waited)
	}
}

// TestLiveEnsureReadyIsIdempotent checks the second command of a session does
// not disturb the container serving the first.
func TestLiveEnsureReadyIsIdempotent(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()

	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if _, err := b.Exec(ctx, liveName, sh("echo first > marker.txt")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := b.EnsureReady(ctx, liveSpec()); err != nil {
		t.Fatalf("EnsureReady again: %v", err)
	}

	res, err := b.Exec(ctx, liveName, sh("cat marker.txt"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "first\n" {
		t.Errorf("marker.txt = %q: readying a running sandbox replaced it", res.Stdout)
	}
}

func live(found []sandbox.BackendSandbox, name string) (sandbox.BackendState, bool) {
	for _, s := range found {
		if s.Name == name {
			return s.State, true
		}
	}
	return 0, false
}
