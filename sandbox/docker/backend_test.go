package docker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/sandbox"
)

const testImage = "alpine:test"

func spec(name string) sandbox.Spec {
	return sandbox.Spec{Name: name, Image: testImage}
}

// TestEnsureReadyCreatesAndStartsAnAbsentSandbox is the first-command case: a
// name nothing exists under becomes a running container, marked as ours so a
// later restart can find it again.
func TestEnsureReadyCreatesAndStartsAnAbsentSandbox(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	exists, running := d.state(b.container("work"))
	if !exists || !running {
		t.Fatalf("after EnsureReady: exists = %v, running = %v; want both true", exists, running)
	}
	config := d.created(b.container("work"))
	if config == nil {
		t.Fatal("container created with no configuration")
	}
	if config.Labels[Label] != "work" {
		t.Errorf("Labels[%q] = %q, want %q: an unlabelled container cannot be recognised as ours", Label, config.Labels[Label], "work")
	}
	if config.Image != testImage {
		t.Errorf("Image = %q, want %q", config.Image, testImage)
	}
	if config.WorkingDir != DefaultWorkdir {
		t.Errorf("WorkingDir = %q, want %q", config.WorkingDir, DefaultWorkdir)
	}
}

// TestEnsureReadyKeepsTheSandboxAlive checks the container is created with
// something that keeps running, since commands are executed inside a live
// container.
func TestEnsureReadyKeepsTheSandboxAlive(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d, WithKeepalive([]string{"sleep", "infinity"}))

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	config := d.created(b.container("work"))
	if want := []string{"sleep", "infinity"}; !slices.Equal(config.Entrypoint, want) {
		t.Errorf("Entrypoint = %v, want %v", config.Entrypoint, want)
	}
	if len(config.Cmd) != 0 {
		t.Errorf("Cmd = %v, want nothing: the image's own command must not run alongside the keepalive", config.Cmd)
	}
}

// TestEnsureReadyConfiguresAnImmediateStop checks the sandbox is created asking
// to be killed rather than asked to leave. Nothing can signal the keepalive —
// it is PID 1 with no handler — so any graceful stop waits out its whole
// timeout: ten seconds on every idle stop and every destroy, including the ones
// run by hand with `docker rm -f`.
func TestEnsureReadyConfiguresAnImmediateStop(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	config := d.created(b.container("work"))
	if config.StopTimeout == nil || *config.StopTimeout != 0 {
		t.Errorf("StopTimeout = %v, want 0", config.StopTimeout)
	}
}

// TestEnsureReadyPullsAnImageThatIsNotPresent checks a missing image is
// acquired before the container is created. The fake daemon refuses to create a
// container for an image it does not have, and only has it once the pull stream
// has been read to the end, so a pull left half-read fails here.
func TestEnsureReadyPullsAnImageThatIsNotPresent(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d)

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady with a missing image: %v", err)
	}

	if d.count("ImagePull") != 1 {
		t.Errorf("ImagePull calls = %d, want 1 (history: %v)", d.count("ImagePull"), d.history())
	}
	if _, running := d.state(b.container("work")); !running {
		t.Error("the sandbox is not running after its image was pulled")
	}
}

// TestEnsureReadyDoesNotPullAnImagePresentLocally keeps the common path off the
// network.
func TestEnsureReadyDoesNotPullAnImagePresentLocally(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if d.count("ImagePull") != 0 {
		t.Errorf("pulled an image that was already present (history: %v)", d.history())
	}
}

// TestEnsureReadyWakesAStoppedSandbox is the persistence promise from the
// backend's side: a stopped sandbox is started, never recreated, because
// recreating it would take its filesystem with it.
func TestEnsureReadyWakesAStoppedSandbox(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d)
	d.addContainer(b.container("work"), "some/other:image", false, map[string]string{Label: "work"})

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if _, running := d.state(b.container("work")); !running {
		t.Error("a stopped sandbox was not started")
	}
	if d.count("ContainerCreate") != 0 {
		t.Errorf("recreated an existing sandbox (history: %v)", d.history())
	}
}

// TestEnsureReadyLeavesARunningSandboxAlone checks readiness is idempotent: the
// second command of a session must not disturb the container serving the first.
func TestEnsureReadyLeavesARunningSandboxAlone(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d)
	d.addContainer(b.container("work"), testImage, true, map[string]string{Label: "work"})

	if err := b.EnsureReady(context.Background(), spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if d.count("ContainerCreate") != 0 || d.count("ContainerStart") != 0 {
		t.Errorf("a running sandbox should be left alone (history: %v)", d.history())
	}
}

// TestEnsureReadyNeedsAnImageOnlyToCreate checks where the image matters: it is
// creation configuration, so a spec without one cannot make a new sandbox, but
// an existing sandbox does not need it.
func TestEnsureReadyNeedsAnImageOnlyToCreate(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		d := newFakeDaemon()
		b := newBackend(d)

		if err := b.EnsureReady(context.Background(), sandbox.Spec{Name: "work"}); err == nil {
			t.Error("creating a sandbox with no image should be rejected")
		}
		if d.count("ContainerCreate") != 0 {
			t.Errorf("tried to create a container with no image (history: %v)", d.history())
		}
	})

	t.Run("existing", func(t *testing.T) {
		d := newFakeDaemon()
		b := newBackend(d)
		d.addContainer(b.container("work"), testImage, false, map[string]string{Label: "work"})

		if err := b.EnsureReady(context.Background(), sandbox.Spec{Name: "work"}); err != nil {
			t.Errorf("an existing sandbox needs no image, got: %v", err)
		}
		if _, running := d.state(b.container("work")); !running {
			t.Error("the existing sandbox was not started")
		}
	})
}

// TestEnsureReadyReportsDaemonFailures checks every daemon call the readiness
// path makes is a failure the caller hears about, rather than a sandbox
// reported ready that is not.
func TestEnsureReadyReportsDaemonFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, method := range []string{"ContainerInspect", "ImageInspect", "ImagePull", "ContainerCreate", "ContainerStart"} {
		t.Run(method, func(t *testing.T) {
			d := newFakeDaemon()
			b := newBackend(d)
			d.failOn(method, boom)

			err := b.EnsureReady(context.Background(), spec("work"))
			if !errors.Is(err, boom) {
				t.Errorf("EnsureReady with a failing %s = %v, want it to report boom", method, err)
			}
		})
	}
}

// TestEnsureReadyReportsAMalformedInspectResponse checks a daemon answering with
// something unexpected is an error, not a panic.
func TestEnsureReadyReportsAMalformedInspectResponse(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d)
	d.addContainer(b.container("work"), testImage, false, map[string]string{Label: "work"})
	d.inspectEmpty = true

	if err := b.EnsureReady(context.Background(), spec("work")); err == nil {
		t.Error("an inspect response with no state should be an error")
	}
}

// TestRejectsNamesThatAreNotUsableContainerNames checks a sandbox name stays a
// name: anything that could not be a container name is refused rather than
// mangled into one, and no operation touches the daemon with it.
func TestRejectsNamesThatAreNotUsableContainerNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../escape", "nested/work", "/abs", "-lead", "has space", "Ünïcode"} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDaemon()
			d.addImage(testImage)
			b := newBackend(d)
			ctx := context.Background()

			if err := b.EnsureReady(ctx, spec(name)); err == nil {
				t.Errorf("EnsureReady(%q) should be rejected", name)
			}
			if _, err := b.Exec(ctx, name, agent.Command{Cmd: "true"}); err == nil {
				t.Errorf("Exec(%q) should be rejected", name)
			}
			if err := b.Stop(ctx, name); err == nil {
				t.Errorf("Stop(%q) should be rejected", name)
			}
			if err := b.Destroy(ctx, name); err == nil {
				t.Errorf("Destroy(%q) should be rejected", name)
			}
			if len(d.history()) != 0 {
				t.Errorf("an unusable name reached the daemon: %v", d.history())
			}
		})
	}
}

// TestAcceptsUsableNames guards the rejection above from becoming too eager.
func TestAcceptsUsableNames(t *testing.T) {
	for _, name := range []string{"work", "work-1", "agent.2", "a_b", "0"} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDaemon()
			d.addImage(testImage)
			b := newBackend(d)

			if err := b.EnsureReady(context.Background(), spec(name)); err != nil {
				t.Errorf("EnsureReady(%q): %v", name, err)
			}
		})
	}
}

// TestStopReleasesComputeAndKeepsTheSandbox checks stopping is not destroying:
// the container stops existing as compute, not as a sandbox.
func TestStopReleasesComputeAndKeepsTheSandbox(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)
	ctx := context.Background()
	if err := b.EnsureReady(ctx, spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if err := b.Stop(ctx, "work"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	exists, running := d.state(b.container("work"))
	if !exists {
		t.Error("Stop removed the sandbox; only Destroy may do that")
	}
	if running {
		t.Error("Stop left the sandbox running")
	}
}

// TestStopDoesNotWaitForTheKeepalive checks compute is released at once. The
// keepalive is the container's PID 1 with no signal handler, so it never stops
// gracefully: asking it to would spend the daemon's whole timeout on every
// single idle stop.
func TestStopDoesNotWaitForTheKeepalive(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)
	ctx := context.Background()
	if err := b.EnsureReady(ctx, spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if err := b.Stop(ctx, "work"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	opts := d.stopOptions()
	if len(opts) != 1 {
		t.Fatalf("ContainerStop calls = %d, want 1", len(opts))
	}
	if opts[0].Timeout == nil || *opts[0].Timeout != 0 {
		t.Errorf("stop timeout = %v, want 0", opts[0].Timeout)
	}
}

// TestStopOfAnAbsentSandboxIsSuccess checks the idempotence the manager relies
// on: a sandbox with no compute is already in the state Stop asks for.
func TestStopOfAnAbsentSandboxIsSuccess(t *testing.T) {
	b := newBackend(newFakeDaemon())
	if err := b.Stop(context.Background(), "never-existed"); err != nil {
		t.Errorf("stopping a sandbox that is not there should be success, got: %v", err)
	}
}

// TestSandboxSurvivesStopAndWake is the persistence promise as the backend sees
// it: waking is starting the same container, so the filesystem cannot be lost on
// the way.
func TestSandboxSurvivesStopAndWake(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)
	ctx := context.Background()

	if err := b.EnsureReady(ctx, spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if err := b.Stop(ctx, "work"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := b.EnsureReady(ctx, spec("work")); err != nil {
		t.Fatalf("EnsureReady after Stop: %v", err)
	}

	if _, running := d.state(b.container("work")); !running {
		t.Error("the sandbox did not wake")
	}
	if n := d.count("ContainerCreate"); n != 1 {
		t.Errorf("ContainerCreate calls = %d, want 1: waking a sandbox must not replace it", n)
	}
}

// TestDestroyRemovesTheSandbox checks destruction takes the filesystem with it,
// even while the sandbox is running, and that repeating it is success.
func TestDestroyRemovesTheSandbox(t *testing.T) {
	d := newFakeDaemon()
	d.addImage(testImage)
	b := newBackend(d)
	ctx := context.Background()
	if err := b.EnsureReady(ctx, spec("work")); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	if err := b.Destroy(ctx, "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if exists, _ := d.state(b.container("work")); exists {
		t.Error("Destroy left the sandbox behind")
	}

	if err := b.Destroy(ctx, "work"); err != nil {
		t.Errorf("destroying an absent sandbox should be success, got: %v", err)
	}
}

// TestLifecycleReportsDaemonFailures checks a lifecycle change that did not
// happen is never reported as if it had.
func TestLifecycleReportsDaemonFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		method string
		call   func(*Backend) error
	}{
		{"ContainerStop", func(b *Backend) error { return b.Stop(context.Background(), "work") }},
		{"ContainerRemove", func(b *Backend) error { return b.Destroy(context.Background(), "work") }},
		{"ContainerList", func(b *Backend) error { _, err := b.List(context.Background()); return err }},
	} {
		t.Run(tc.method, func(t *testing.T) {
			d := newFakeDaemon()
			d.failOn(tc.method, boom)

			if err := tc.call(newBackend(d)); !errors.Is(err, boom) {
				t.Errorf("a failing %s reported %v, want boom", tc.method, err)
			}
		})
	}
}

// TestListReportsOnlyOurSandboxes checks what a restarted harness learns from
// the daemon: the sandboxes this backend owns, under the names it opened them
// with, and nothing else that happens to be on the host.
func TestListReportsOnlyOurSandboxes(t *testing.T) {
	d := newFakeDaemon()
	b := newBackend(d)
	d.addContainer(b.container("beta"), "beta:1", true, map[string]string{Label: "beta"})
	d.addContainer(b.container("alpha"), "alpha:1", false, map[string]string{Label: "alpha"})
	// Somebody else's container, and one that only looks like ours.
	d.addContainer("someones-database", "postgres:17", true, nil)
	d.addContainer(b.container("impostor"), "impostor:1", true, map[string]string{"other.label": "x"})

	found, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := []sandbox.BackendSandbox{
		{Name: "alpha", Image: "alpha:1", State: sandbox.BackendStopped},
		{Name: "beta", Image: "beta:1", State: sandbox.BackendRunning},
	}
	if !slices.Equal(found, want) {
		t.Errorf("List() = %+v, want %+v", found, want)
	}
}

// TestListReportsNothingWhenThereAreNoSandboxes checks a fresh host is an empty
// backend rather than a failure to start up.
func TestListReportsNothingWhenThereAreNoSandboxes(t *testing.T) {
	found, err := newBackend(newFakeDaemon()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("List() = %+v, want nothing", found)
	}
}

var _ sandbox.Backend = (*Backend)(nil)
