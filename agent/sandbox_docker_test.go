package agent_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

// recorder collects sandbox events so tests can assert what was reported.
type recorder struct{ events []agent.SandboxEvent }

func (r *recorder) observe(ev agent.SandboxEvent) { r.events = append(r.events, ev) }

func (r *recorder) types() []agent.SandboxEventType {
	out := make([]agent.SandboxEventType, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Type
	}
	return out
}

func (r *recorder) find(t agent.SandboxEventType) (agent.SandboxEvent, bool) {
	for _, ev := range r.events {
		if ev.Type == t {
			return ev, true
		}
	}
	return agent.SandboxEvent{}, false
}

func bashCmd(script string) agent.Command {
	return agent.Command{Cmd: "bash", Args: []string{"-c", script}}
}

// TestDockerEphemeralSandboxLifecycle pins the whole ephemeral story: a
// throwaway container is created with a keepalive entrypoint and the workdir
// bind-mounted, it is running by the time Acquire returns, commands exec into
// it, and Close reclaims it — today's LocalFactory semantics, in a container.
func TestDockerEphemeralSandboxLifecycle(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Image: "golang:1.26", Mount: "/host/work", Dir: "/work", Client: d}

	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if len(d.containers) != 1 {
		t.Fatalf("want exactly one container created, got %v", d.names())
	}
	c := d.containers[0]
	switch {
	case c.image != "golang:1.26":
		t.Errorf("container image = %q, want golang:1.26", c.image)
	case !slices.Equal(c.cmd, []string{"sleep", "infinity"}):
		t.Errorf("container cmd = %q, want a keepalive so exec has something to enter", c.cmd)
	case !slices.Equal(c.binds, []string{"/host/work:/work"}):
		t.Errorf("container binds = %q, want the host workdir mounted at Dir", c.binds)
	case c.workDir != "/work":
		t.Errorf("container workdir = %q, want /work", c.workDir)
	case c.labels["metaharness.durable"] != "false":
		t.Errorf("container labels = %v, want metaharness.durable=false", c.labels)
	case !c.autoRemove:
		t.Error("a throwaway container must auto-remove, so the daemon reclaims it even if Close never runs")
	case !c.running:
		t.Error("Acquire must leave the container running, or exec has nothing to enter")
	}

	if _, err := box.Exec(t.Context(), bashCmd("echo hi")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := d.lastExec(); got.container != c.id {
		t.Errorf("exec targeted %q, want the created container %q", got.container, c.id)
	} else if !slices.Equal(got.cmd, []string{"bash", "-c", "echo hi"}) {
		t.Errorf("exec cmd = %q, want the command and its args", got.cmd)
	}

	if err := box.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(d.containers) != 0 {
		t.Errorf("an ephemeral container must be reclaimed by Close, still there: %v", d.names())
	}
}

// TestDockerEphemeralNameIsAdvisoryLabel checks a non-durable spec's name is
// passed as a label for a readable `docker ps` but never used for adoption: an
// existing container of that name is left alone and a fresh one is created.
func TestDockerEphemeralNameIsAdvisoryLabel(t *testing.T) {
	d := &fakeDocker{}
	existing := d.seedSandbox("scratch", "golang:1.26", true)
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "scratch"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if len(d.containers) != 2 {
		t.Fatalf("a non-durable spec must create its own container, got %v", d.names())
	}
	fresh := d.containers[1]
	if fresh == existing || fresh.name == "scratch" {
		t.Errorf("a non-durable spec must not adopt or reuse the name, got %+v", fresh)
	}
	if fresh.labels["metaharness.sandbox"] != "scratch" {
		t.Errorf("labels = %v, want the name carried as metaharness.sandbox for a readable docker ps", fresh.labels)
	}
	if fresh.labels["metaharness.durable"] != "false" {
		t.Errorf("labels = %v, want metaharness.durable=false", fresh.labels)
	}
}

// TestDockerAcquireCreatesAbsentDurableSandbox covers the "absent" row of the
// adopt-or-create table: nothing with that name exists, so a named, labelled
// container is created — and without auto-remove, because it must survive its
// handle.
func TestDockerAcquireCreatesAbsentDurableSandbox(t *testing.T) {
	d := &fakeDocker{}
	rec := &recorder{}
	f := agent.DockerFactory{Image: "golang:1.26", Mount: "/host/work", Dir: "/work", Client: d, Observer: rec.observe}

	box, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if !slices.Equal(d.names(), []string{"work"}) {
		t.Fatalf("containers = %v, want one named work", d.names())
	}
	c := d.containers[0]
	switch {
	case c.image != "golang:1.26":
		t.Errorf("container image = %q, want golang:1.26", c.image)
	case c.labels["metaharness.sandbox"] != "work" || c.labels["metaharness.durable"] != "true":
		t.Errorf("container labels = %v, want the sandbox name and durable=true", c.labels)
	case !slices.Equal(c.binds, []string{"/host/work:/work"}):
		t.Errorf("container binds = %q, want the host workdir mounted at Dir", c.binds)
	case c.autoRemove:
		t.Error("a durable container must not auto-remove: it has to outlive its handle")
	case !c.running:
		t.Error("Acquire must leave the container running")
	}
	if _, ok := rec.find(agent.SandboxAdopted); ok {
		t.Errorf("creating a sandbox must not report adoption: %v", rec.types())
	}

	// The handle addresses the container by name, which is its identity.
	if _, err := box.Exec(t.Context(), bashCmd("true")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := d.lastExec().container; got != "work" {
		t.Errorf("exec targeted %q, want the container name", got)
	}
}

// TestDockerAcquireAdoptsRunningSandbox covers the "running" row: an existing
// running container is attached to as-is — not recreated, not restarted — and
// the adoption is reported.
func TestDockerAcquireAdoptsRunningSandbox(t *testing.T) {
	d := &fakeDocker{}
	existing := d.seedSandbox("work", "golang:1.26", true)
	rec := &recorder{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d, Observer: rec.observe}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Image: "golang:1.26", Durable: true}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if len(d.containers) != 1 || d.containers[0] != existing {
		t.Fatalf("the running container must be attached to, not replaced; containers = %v", d.names())
	}
	if slices.Contains(d.calls, "ContainerStart") {
		t.Errorf("an already running container must not be started again, calls = %v", d.calls)
	}
	ev, ok := rec.find(agent.SandboxAdopted)
	if !ok {
		t.Fatalf("no adopted event reported, got %v", rec.types())
	}
	if ev.Name != "work" || ev.Image != "golang:1.26" {
		t.Errorf("adopted event = %+v, want name work running golang:1.26", ev)
	}
}

// TestDockerAcquireStartsStoppedSandbox covers the "stopped" row: the container
// exists with its filesystem intact, so it is started and attached to rather
// than replaced.
func TestDockerAcquireStartsStoppedSandbox(t *testing.T) {
	d := &fakeDocker{}
	existing := d.seedSandbox("work", "golang:1.26", false)
	rec := &recorder{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d, Observer: rec.observe}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if len(d.containers) != 1 || d.containers[0] != existing {
		t.Fatalf("a stopped container must be started, not replaced; containers = %v", d.names())
	}
	if !existing.running {
		t.Error("the adopted container is still stopped, so no command could run in it")
	}
	if _, ok := rec.find(agent.SandboxAdopted); !ok {
		t.Errorf("starting an existing sandbox is an adoption, got events %v", rec.types())
	}
}

// TestDockerAcquireImageMismatchAttachesAnyway covers the "image mismatch" row:
// the name is the identity, so an adopted container built from another image is
// still attached to — but the discrepancy is reported rather than swallowed.
func TestDockerAcquireImageMismatchAttachesAnyway(t *testing.T) {
	d := &fakeDocker{}
	existing := d.seedSandbox("work", "golang:1.25", true)
	rec := &recorder{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d, Observer: rec.observe}

	box, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Image: "golang:1.26", Durable: true})
	if err != nil {
		t.Fatalf("Acquire must attach despite an image mismatch: %v", err)
	}
	if _, err := box.Exec(t.Context(), bashCmd("true")); err != nil {
		t.Fatalf("Exec on the adopted sandbox: %v", err)
	}
	if len(d.containers) != 1 || d.containers[0] != existing {
		t.Errorf("a mismatched image must not be recreated, containers = %v", d.names())
	}

	ev, ok := rec.find(agent.SandboxImageMismatch)
	if !ok {
		t.Fatalf("no image_mismatch event reported, got %v", rec.types())
	}
	if ev.Name != "work" || ev.Image != "golang:1.25" || ev.WantImage != "golang:1.26" {
		t.Errorf("image_mismatch event = %+v, want work: has golang:1.25, spec asked golang:1.26", ev)
	}
}

// TestDockerAcquireMatchingImageIsNotAMismatch guards the mismatch report
// against firing on the happy path.
func TestDockerAcquireMatchingImageIsNotAMismatch(t *testing.T) {
	d := &fakeDocker{}
	d.seedSandbox("work", "golang:1.26", true)
	rec := &recorder{}
	f := agent.DockerFactory{Client: d, Observer: rec.observe}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Image: "golang:1.26", Durable: true}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if ev, ok := rec.find(agent.SandboxImageMismatch); ok {
		t.Errorf("unexpected image_mismatch for a matching image: %+v", ev)
	}
}

// TestDockerSpecImageOverridesFactoryImage pins the precedence: the spec is the
// request, the factory field is only the default.
func TestDockerSpecImageOverridesFactoryImage(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Image: "python:3.14"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := d.containers[0].image; got != "python:3.14" {
		t.Errorf("container image = %q, want the spec's image", got)
	}
}

// TestDockerDurableWithoutNameIsAnError pins that durability needs an identity:
// there is nothing to adopt or destroy without a name.
func TestDockerDurableWithoutNameIsAnError(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	if _, err := f.Acquire(t.Context(), agent.SandboxSpec{Durable: true}); !errors.Is(err, agent.ErrDurableNeedsName) {
		t.Fatalf("Acquire error = %v, want ErrDurableNeedsName", err)
	}
	if len(d.calls) != 0 {
		t.Errorf("a rejected spec must not touch docker, got %v", d.calls)
	}
}

// TestDockerAcquireWithoutAnImageIsAnError pins that a spec with no image, and
// no factory default to fall back on, is rejected by name before anything is
// created — rather than reaching the daemon with an empty image reference.
func TestDockerAcquireWithoutAnImageIsAnError(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Dir: "/work", Client: d}

	_, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true})
	if err == nil {
		t.Fatal("expected an error for a spec with no image and no factory default")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error %q does not name the sandbox it is about", err)
	}
	if len(d.containers) != 0 {
		t.Errorf("nothing may be created without an image, got %v", d.names())
	}
}

// TestDockerAcquireReportsAnUnanswerableDaemon pins that only a genuine "no such
// container" means absent. Any other lookup failure is the daemon's problem and
// is reported as-is, rather than being papered over by creating a second
// container next to one that may well already exist.
func TestDockerAcquireReportsAnUnanswerableDaemon(t *testing.T) {
	boom := errors.New("daemon unreachable")
	d := &fakeDocker{fail: map[string]error{"ContainerInspect": boom}}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	_, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true})
	if !errors.Is(err, boom) {
		t.Fatalf("Acquire error = %v, want the daemon's own failure", err)
	}
	if len(d.containers) != 0 {
		t.Errorf("nothing may be created when the daemon cannot be asked, got %v", d.names())
	}
}

// TestDockerExecPreservesStderrOnSuccess is the reason this backend talks to the
// API instead of the CLI: a command that exits zero still has stderr, and
// tools/bash.go shows it to the model. sandbox.Local always reports it, so the
// docker backend must too — otherwise `git clone`, `npm install` and every
// compiler warning look silent in a container and chatty on the host.
func TestDockerExecPreservesStderrOnSuccess(t *testing.T) {
	d := &fakeDocker{}
	d.answer(fakeRun{Frames: []fakeFrame{
		outFrame("HEAD is now at 1234567\n"),
		errFrame("Cloning into 'repo'...\n"),
	}})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}
	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	res, err := box.Exec(t.Context(), bashCmd("git clone repo"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExecResult = %+v, want a successful command", res)
	}
	if res.Stderr != "Cloning into 'repo'...\n" {
		t.Errorf("Stderr = %q, want the stderr of a command that exited zero", res.Stderr)
	}
	if res.Stdout != "HEAD is now at 1234567\n" {
		t.Errorf("Stdout = %q, want the command's stdout", res.Stdout)
	}
}

// TestDockerExecKeepsStdoutAndStderrApart pins the demultiplexing itself: the
// daemon sends one interleaved stream, and the two halves must come back whole,
// in order, and on the right side.
func TestDockerExecKeepsStdoutAndStderrApart(t *testing.T) {
	d := &fakeDocker{}
	d.answer(fakeRun{Frames: []fakeFrame{
		outFrame("one\n"),
		errFrame("warn one\n"),
		outFrame("two\n"),
		errFrame("warn two\n"),
	}})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}
	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	res, err := box.Exec(t.Context(), bashCmd("noisy"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "one\ntwo\n" {
		t.Errorf("Stdout = %q, want only the stdout writes, in order", res.Stdout)
	}
	if res.Stderr != "warn one\nwarn two\n" {
		t.Errorf("Stderr = %q, want only the stderr writes, in order", res.Stderr)
	}
}

// TestDockerExecReportsExitCode pins the same contract sandbox.Local has: a
// command that ran and failed is a populated result with a nil error, so the
// model sees the failure instead of the run aborting.
func TestDockerExecReportsExitCode(t *testing.T) {
	d := &fakeDocker{}
	d.answer(fakeRun{
		Frames: []fakeFrame{outFrame("partial output\n"), errFrame("boom\n")},
		Exit:   3,
	})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}
	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	res, err := box.Exec(t.Context(), bashCmd("exit 3"))
	if err != nil {
		t.Fatalf("a non-zero exit is not an infra error, got: %v", err)
	}
	if res.ExitCode != 3 || res.Stdout != "partial output\n" || res.Stderr != "boom\n" {
		t.Errorf("ExecResult = %+v, want exit 3 with stdout and stderr preserved", res)
	}
}

// TestDockerExecOnAStoppedContainerIsAnError is the other half of that contract,
// and the one the CLI seam could not express: the sandbox losing its compute —
// a daemon restart, an OOM kill, a stray `docker stop` — must not look like the
// guest command exiting non-zero, or the registry keeps believing the sandbox is
// awake and never wakes it.
func TestDockerExecOnAStoppedContainerIsAnError(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}
	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	d.containers[0].running = false

	res, err := box.Exec(t.Context(), bashCmd("true"))
	if err == nil {
		t.Fatalf("a stopped container must be an infra error, got result %+v", res)
	}
	if res != (agent.ExecResult{}) {
		t.Errorf("ExecResult = %+v, want a zero result: this is not a command that ran", res)
	}
}

// TestDockerExecOnAMissingContainerIsAnError pins the same for a container that
// is gone entirely.
func TestDockerExecOnAMissingContainerIsAnError(t *testing.T) {
	d := &fakeDocker{}
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}
	box, err := f.Acquire(t.Context(), agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	d.containers = nil

	res, err := box.Exec(t.Context(), bashCmd("true"))
	if err == nil {
		t.Fatalf("a missing container must be an infra error, got result %+v", res)
	}
	if res != (agent.ExecResult{}) {
		t.Errorf("ExecResult = %+v, want a zero result: this is not a command that ran", res)
	}
}

// TestDockerSleepStopsAndWakeStarts pins sleep as stop-compute-keep-disk and
// wake as its inverse.
func TestDockerSleepStopsAndWakeStarts(t *testing.T) {
	d := &fakeDocker{}
	c := d.seedSandbox("work", "golang:1.26", true)
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	box, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sleeper, ok := box.(agent.Sleeper)
	if !ok {
		t.Fatal("a docker sandbox must implement agent.Sleeper")
	}

	if err := sleeper.Sleep(t.Context()); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if c.running {
		t.Error("Sleep must release the container's compute")
	}
	if len(d.containers) != 1 {
		t.Errorf("Sleep must keep the filesystem, containers = %v", d.names())
	}

	if err := sleeper.Wake(t.Context()); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if !c.running {
		t.Error("Wake must make the container runnable again")
	}
}

// TestDockerCloseDetachesDurableSandbox is the heart of the direction: nothing
// in the agent loop destroys a durable sandbox, so Close leaves it running.
func TestDockerCloseDetachesDurableSandbox(t *testing.T) {
	d := &fakeDocker{}
	c := d.seedSandbox("work", "golang:1.26", true)
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	box, err := f.Acquire(t.Context(), agent.SandboxSpec{Name: "work", Durable: true})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(d.containers) != 1 || !c.running {
		t.Errorf("Close on a durable sandbox must detach only; containers = %v, running = %v", d.names(), c.running)
	}
}

// TestDockerDestroyRemovesByName pins the one path that does remove a durable
// sandbox: an explicit destroy. It removes a running one, too.
func TestDockerDestroyRemovesByName(t *testing.T) {
	d := &fakeDocker{}
	d.seedSandbox("work", "golang:1.26", true)
	d.seedSandbox("other", "golang:1.26", true)
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	if err := f.Destroy(t.Context(), "work"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !slices.Equal(d.names(), []string{"other"}) {
		t.Errorf("containers = %v, want only work removed", d.names())
	}
}

// TestDockerListReportsGroundTruth pins that the backend, not a database, is
// the source of truth for which sandboxes exist, whether they are awake, and
// what image they actually run.
func TestDockerListReportsGroundTruth(t *testing.T) {
	d := &fakeDocker{}
	d.seedSandbox("work", "golang:1.26", true)
	d.seedSandbox("old", "golang:1.25", false)
	d.seed(&fakeContainer{
		name: "scratch", image: "bash:5", running: true,
		labels: map[string]string{"metaharness.durable": "false"},
	})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	got, err := f.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []agent.SandboxInfo{
		{Name: "work", State: agent.SandboxAwake, Durable: true, Image: "golang:1.26"},
		{Name: "old", State: agent.SandboxAsleep, Durable: true, Image: "golang:1.25"},
		{Name: "scratch", State: agent.SandboxAwake, Durable: false, Image: "bash:5"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("List =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestDockerListFindsUnnamedThrowawaysAndIgnoresStrangers pins which label the
// listing filters on. Reconciliation exists to reap throwaway leftovers, and a
// throwaway created from a spec with no name carries no metaharness.sandbox
// label at all — so filtering on that label would silently miss exactly the
// containers reconciliation is for. metaharness.durable is on everything this
// harness creates, and on nothing it did not.
func TestDockerListFindsUnnamedThrowawaysAndIgnoresStrangers(t *testing.T) {
	d := &fakeDocker{}
	d.seed(&fakeContainer{
		name: "wistful_bardeen", image: "bash:5", running: true,
		labels: map[string]string{"metaharness.durable": "false"},
	})
	d.seed(&fakeContainer{name: "postgres", image: "postgres:18", running: true})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	got, err := f.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []agent.SandboxInfo{
		{Name: "wistful_bardeen", State: agent.SandboxAwake, Durable: false, Image: "bash:5"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("List =\n  %+v\nwant\n  %+v\n(an unnamed throwaway must be listed; a stranger's container must not)", got, want)
	}
}

// TestDockerListSurvivesANamelessContainer keeps the ground truth complete: a
// container the daemon reports without a name is still addressable by id, and
// dropping it from the listing would hide it from reconciliation forever.
func TestDockerListSurvivesANamelessContainer(t *testing.T) {
	d := &fakeDocker{}
	c := d.seed(&fakeContainer{
		image: "bash:5", running: false,
		labels: map[string]string{"metaharness.durable": "false"},
	})
	f := agent.DockerFactory{Image: "golang:1.26", Client: d}

	got, err := f.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []agent.SandboxInfo{{Name: c.id, State: agent.SandboxAsleep, Image: "bash:5"}}
	if !slices.Equal(got, want) {
		t.Errorf("List =\n  %+v\nwant\n  %+v", got, want)
	}
}
