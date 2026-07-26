package docker

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/maikdotfi/metaharness/agent"
)

// running gives a backend one running sandbox, which is what Exec requires.
func running(t *testing.T, d *fakeDaemon, name string) *Backend {
	t.Helper()
	b := newBackend(d)
	if err := b.EnsureReady(context.Background(), spec(name)); err != nil {
		t.Fatalf("EnsureReady(%q): %v", name, err)
	}
	return b
}

func frame(stream stdcopy.StdType, payload string) []byte {
	var buf bytes.Buffer
	stdcopy.NewStdWriter(&buf, stream).Write([]byte(payload))
	return buf.Bytes()
}

// TestExecKeepsBothStreams checks stdout and stderr are captured separately, so
// a caller can tell what a command said from where it said it.
func TestExecKeepsBothStreams(t *testing.T) {
	d := newFakeDaemon()
	d.script("sh -c report", fakeRun{Stdout: "out\n", Stderr: "err\n"})
	b := running(t, d, "work")

	res, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "report"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "out\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "err\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestExecReportsAFailedCommandAsAResult is the boundary the whole contract
// rests on: a command that ran and exited non-zero is an outcome the caller
// inspects, not a failure of the sandbox, and its output still comes back.
func TestExecReportsAFailedCommandAsAResult(t *testing.T) {
	d := newFakeDaemon()
	d.script("sh -c fail", fakeRun{Stderr: "no such file\n", Exit: 2})
	b := running(t, d, "work")

	res, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "fail"}})
	if err != nil {
		t.Fatalf("a non-zero exit is not an infra error, got: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", res.ExitCode)
	}
	if res.Stderr != "no such file\n" {
		t.Errorf("Stderr = %q, want the command's output", res.Stderr)
	}
}

// TestExecRunsTheCommandInTheWorkdir checks the command reaches the daemon whole
// and lands in the sandbox's working directory.
func TestExecRunsTheCommandInTheWorkdir(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d, WithWorkdir("/srv/box"))
	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	cmd := agent.Command{Cmd: "bash", Args: []string{"-c", "echo hi"}}
	if _, err := b.Exec(context.Background(), "work", cmd); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	e := d.lastExec()
	if e == nil {
		t.Fatal("no exec was created")
	}
	if want := []string{"bash", "-c", "echo hi"}; !slices.Equal(e.cmd, want) {
		t.Errorf("exec Cmd = %v, want %v", e.cmd, want)
	}
	if e.workdir != "/srv/box" {
		t.Errorf("exec WorkingDir = %q, want %q", e.workdir, "/srv/box")
	}
}

// TestExecReassemblesOutputSplitAcrossFrames checks the demultiplexing is real:
// a daemon frames output as it comes, interleaved and in whatever sizes it
// likes, and each stream has to arrive whole and in order.
func TestExecReassemblesOutputSplitAcrossFrames(t *testing.T) {
	var stream []byte
	for _, part := range []struct {
		which stdcopy.StdType
		text  string
	}{
		{stdcopy.Stdout, "first "},
		{stdcopy.Stderr, "warning "},
		{stdcopy.Stdout, "second "},
		{stdcopy.Stderr, "again"},
		{stdcopy.Stdout, "third"},
	} {
		stream = append(stream, frame(part.which, part.text)...)
	}

	d := newFakeDaemon()
	d.script("sh -c chatty", fakeRun{Frames: stream, Exit: 7})
	b := running(t, d, "work")

	res, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "chatty"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "first second third" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "first second third")
	}
	if res.Stderr != "warning again" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "warning again")
	}
	// The exit status is only knowable once the stream has ended, so getting it
	// right here is also what proves the stream was drained first.
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7: the exit status was read before the stream ended", res.ExitCode)
	}
}

// TestExecKeepsOutputLargerThanOneRead checks a command that produces a lot —
// building, testing, listing a tree — does not have its output truncated.
func TestExecKeepsOutputLargerThanOneRead(t *testing.T) {
	big := strings.Repeat("a line of output\n", 8000)

	d := newFakeDaemon()
	d.script("sh -c verbose", fakeRun{Stdout: big})
	b := running(t, d, "work")

	res, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "verbose"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != big {
		t.Errorf("Stdout is %d bytes, want %d", len(res.Stdout), len(big))
	}
}

// TestExecReportsADaemonErrorInTheStream checks a daemon that fails midway
// through the output says so, rather than the truncated output passing for the
// command's result.
func TestExecReportsADaemonErrorInTheStream(t *testing.T) {
	stream := append(frame(stdcopy.Stdout, "partial"), frame(stdcopy.Systemerr, "daemon gave up")...)

	d := newFakeDaemon()
	d.script("sh -c doomed", fakeRun{Frames: stream})
	b := running(t, d, "work")

	if _, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "doomed"}}); err == nil {
		t.Error("an error in the output stream should be reported, not treated as output")
	}
}

// TestExecReportsAMalformedAttachResponse checks a daemon answering with
// something the client cannot read is an error, not a panic.
func TestExecReportsAMalformedAttachResponse(t *testing.T) {
	d := newFakeDaemon()
	b := running(t, d, "work")
	d.attachRaw = true

	if _, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "true"}); err == nil {
		t.Error("an attach response with no connection should be an error")
	}
}

// TestExecReportsAnUnfinishedExit checks the one case where the daemon leaves
// the outcome unknown. Reporting a zero exit code for a command still running
// would tell the caller it succeeded, which is the worst available answer.
func TestExecReportsAnUnfinishedExit(t *testing.T) {
	d := newFakeDaemon()
	d.script("sh -c odd", fakeRun{Stdout: "hm\n", Exit: 1})
	b := running(t, d, "work")
	d.execNeverFinishes = true

	if _, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "sh", Args: []string{"-c", "odd"}}); err == nil {
		t.Error("an exit status the daemon will not give should be an error")
	}
}

// TestExecStopsWhenTheContextIsCancelled checks cancellation interrupts the read
// of a command still producing output, rather than the caller waiting for a
// command that may never end.
func TestExecStopsWhenTheContextIsCancelled(t *testing.T) {
	started, block := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(block) })

	d := newFakeDaemon()
	d.script("sh -c forever", fakeRun{Stdout: "working\n", Started: started, Block: block})
	b := running(t, d, "work")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	_, err := b.Exec(ctx, "work", agent.Command{Cmd: "sh", Args: []string{"-c", "forever"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec on a cancelled context = %v, want it to report cancellation", err)
	}
}

// TestExecReportsDaemonFailures checks every daemon call in the command path is
// a failure the caller hears about.
func TestExecReportsDaemonFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, method := range []string{"ContainerExecCreate", "ContainerExecAttach", "ContainerExecInspect"} {
		t.Run(method, func(t *testing.T) {
			d := newFakeDaemon()
			b := running(t, d, "work")
			d.failOn(method, boom)

			_, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "true"})
			if !errors.Is(err, boom) {
				t.Errorf("Exec with a failing %s = %v, want it to report boom", method, err)
			}
		})
	}
}

// TestExecNeedsARunningSandbox checks the backend does not invent readiness: a
// sandbox that is stopped or absent cannot run a command, and saying so is what
// lets the manager above make it ready and try again.
func TestExecNeedsARunningSandbox(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		d := newFakeDaemon()
		b := running(t, d, "work")
		if err := b.Stop(context.Background(), "work"); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if _, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "true"}); err == nil {
			t.Error("a stopped sandbox cannot run a command")
		}
	})

	t.Run("absent", func(t *testing.T) {
		d := newFakeDaemon()
		b := newBackend(d)

		if _, err := b.Exec(context.Background(), "work", agent.Command{Cmd: "true"}); err == nil {
			t.Error("a sandbox that does not exist cannot run a command")
		}
	})
}
