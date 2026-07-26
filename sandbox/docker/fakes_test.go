package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDaemon is an in-memory Docker daemon. Containers exist, run, stop and are
// removed, images are present or have to be pulled, and an exec produces a real
// multiplexed stream over a real connection — so the backend's framing,
// draining and cancellation are exercised rather than mocked away.
//
// Two behaviours are modelled deliberately harshly, because both are bug
// classes a lenient fake would hide:
//
//   - creating a container whose image is not present locally fails, and a pull
//     only makes the image present once its progress stream has been read to the
//     end. A backend that fires a pull and creates without draining it fails.
//   - an exec reports itself still running until its output stream has ended, so
//     a backend that reads the exit status before draining gets no status at all.
//
// It ignores list filters and reports every container it has, which is the
// permissive direction: List has to prove it reports only sandboxes it owns.
type fakeDaemon struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	images     map[string]bool
	execs      map[string]*fakeExec
	runs       map[string]fakeRun
	errs       map[string]error
	calls      []string
	pulls      []string
	stopped    []container.StopOptions
	nextID     int

	// attachRaw makes ContainerExecAttach return a response with no connection,
	// as a daemon speaking an unexpected protocol would.
	attachRaw bool

	// inspectEmpty makes ContainerInspect answer with a response holding
	// nothing, as a daemon of an unexpected version would.
	inspectEmpty bool

	// execNeverFinishes makes an exec report itself still running even after its
	// output has ended, leaving the exit status unknowable.
	execNeverFinishes bool
}

type fakeContainer struct {
	image   string
	running bool
	config  *container.Config
	host    *container.HostConfig
}

// fakeExec is one created exec and the output it will produce.
type fakeExec struct {
	name    string
	cmd     []string
	workdir string
	run     fakeRun
	drained bool
}

// fakeRun is what a scripted command produces. Frames holds the exact stream
// bytes when a test needs to control framing itself; otherwise Stdout and
// Stderr are framed normally. Started is closed once the output has been
// written, and Block, when non-nil, then holds the stream open until it is
// closed — together they are a command still running at a moment a test
// chooses.
type fakeRun struct {
	Stdout  string
	Stderr  string
	Exit    int
	Frames  []byte
	Started chan struct{}
	Block   chan struct{}
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		containers: map[string]*fakeContainer{},
		images:     map[string]bool{},
		execs:      map[string]*fakeExec{},
		runs:       map[string]fakeRun{},
		errs:       map[string]error{},
	}
}

var (
	_ daemon = (*fakeDaemon)(nil)
	_ daemon = (*client.Client)(nil)
)

func (d *fakeDaemon) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerInspect", name); err != nil {
		return container.InspectResponse{}, err
	}
	c, ok := d.containers[name]
	if !ok {
		return container.InspectResponse{}, notFound("container", name)
	}
	if d.inspectEmpty {
		return container.InspectResponse{}, nil
	}
	status := container.StateExited
	if c.running {
		status = container.StateRunning
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:    name,
			Name:  "/" + name,
			Image: c.image,
			State: &container.State{Status: status, Running: c.running},
		},
		Config: c.config,
	}, nil
}

func (d *fakeDaemon) ContainerCreate(_ context.Context, config *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerCreate", name); err != nil {
		return container.CreateResponse{}, err
	}
	if _, ok := d.containers[name]; ok {
		return container.CreateResponse{}, fmt.Errorf("fake daemon: container %q already exists: %w", name, cerrdefs.ErrConflict)
	}
	if config == nil || config.Image == "" {
		return container.CreateResponse{}, fmt.Errorf("fake daemon: no image given for %q: %w", name, cerrdefs.ErrInvalidArgument)
	}
	if !d.images[config.Image] {
		return container.CreateResponse{}, fmt.Errorf("fake daemon: no such image %q: %w", config.Image, cerrdefs.ErrNotFound)
	}
	d.containers[name] = &fakeContainer{image: config.Image, config: config, host: host}
	return container.CreateResponse{ID: name}, nil
}

func (d *fakeDaemon) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerStart", name); err != nil {
		return err
	}
	c, ok := d.containers[name]
	if !ok {
		return notFound("container", name)
	}
	c.running = true
	return nil
}

func (d *fakeDaemon) ContainerStop(_ context.Context, name string, opts container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerStop", name); err != nil {
		return err
	}
	d.stopped = append(d.stopped, opts)
	c, ok := d.containers[name]
	if !ok {
		return notFound("container", name)
	}
	c.running = false
	return nil
}

func (d *fakeDaemon) ContainerRemove(_ context.Context, name string, _ container.RemoveOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerRemove", name); err != nil {
		return err
	}
	if _, ok := d.containers[name]; !ok {
		return notFound("container", name)
	}
	delete(d.containers, name)
	return nil
}

// ContainerList reports every container, filters and all, so that the backend's
// ownership filter is what a test measures.
func (d *fakeDaemon) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerList", ""); err != nil {
		return nil, err
	}
	found := make([]container.Summary, 0, len(d.containers))
	for name, c := range d.containers {
		state := container.StateExited
		if c.running {
			state = container.StateRunning
		}
		var labels map[string]string
		if c.config != nil {
			labels = c.config.Labels
		}
		found = append(found, container.Summary{
			ID:     name,
			Names:  []string{"/" + name},
			Image:  c.image,
			State:  state,
			Labels: labels,
		})
	}
	slices.SortFunc(found, func(a, b container.Summary) int { return strings.Compare(a.ID, b.ID) })
	return found, nil
}

func (d *fakeDaemon) ContainerExecCreate(_ context.Context, name string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerExecCreate", name); err != nil {
		return container.ExecCreateResponse{}, err
	}
	c, ok := d.containers[name]
	if !ok {
		return container.ExecCreateResponse{}, notFound("container", name)
	}
	if !c.running {
		return container.ExecCreateResponse{}, fmt.Errorf("fake daemon: container %q is not running: %w", name, cerrdefs.ErrConflict)
	}
	d.nextID++
	id := fmt.Sprintf("exec-%d", d.nextID)
	d.execs[id] = &fakeExec{
		name:    name,
		cmd:     slices.Clone(opts.Cmd),
		workdir: opts.WorkingDir,
		run:     d.runs[strings.Join(opts.Cmd, " ")],
	}
	return container.ExecCreateResponse{ID: id}, nil
}

// ContainerExecAttach hands back a real connection and writes the scripted
// output into it from another goroutine, exactly as the daemon streams it.
func (d *fakeDaemon) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	d.mu.Lock()
	if err := d.begin("ContainerExecAttach", id); err != nil {
		d.mu.Unlock()
		return types.HijackedResponse{}, err
	}
	e, ok := d.execs[id]
	raw := d.attachRaw
	d.mu.Unlock()

	if !ok {
		return types.HijackedResponse{}, notFound("exec", id)
	}
	if raw {
		return types.HijackedResponse{}, nil
	}

	near, far := net.Pipe()
	go func() {
		defer far.Close()
		d.stream(e, far)
	}()
	return types.NewHijackedResponse(near, "application/vnd.docker.multiplexed-stream"), nil
}

// stream writes one exec's output and then marks it drained, before closing, so
// a backend that reads the exit status only after the stream ends always finds
// the exec finished.
func (d *fakeDaemon) stream(e *fakeExec, w io.Writer) {
	run := e.run
	if run.Frames != nil {
		w.Write(run.Frames)
	} else {
		if run.Stdout != "" {
			stdcopy.NewStdWriter(w, stdcopy.Stdout).Write([]byte(run.Stdout))
		}
		if run.Stderr != "" {
			stdcopy.NewStdWriter(w, stdcopy.Stderr).Write([]byte(run.Stderr))
		}
	}
	if run.Started != nil {
		close(run.Started)
	}
	if run.Block != nil {
		<-run.Block
	}

	d.mu.Lock()
	e.drained = true
	d.mu.Unlock()
}

func (d *fakeDaemon) ContainerExecInspect(_ context.Context, id string) (container.ExecInspect, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ContainerExecInspect", id); err != nil {
		return container.ExecInspect{}, err
	}
	e, ok := d.execs[id]
	if !ok {
		return container.ExecInspect{}, notFound("exec", id)
	}
	if !e.drained || d.execNeverFinishes {
		return container.ExecInspect{ExecID: id, ContainerID: e.name, Running: true}, nil
	}
	return container.ExecInspect{ExecID: id, ContainerID: e.name, ExitCode: e.run.Exit}, nil
}

func (d *fakeDaemon) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ImageInspect", ref); err != nil {
		return image.InspectResponse{}, err
	}
	if !d.images[ref] {
		return image.InspectResponse{}, notFound("image", ref)
	}
	return image.InspectResponse{ID: "sha256:" + ref}, nil
}

// ImagePull returns a progress stream, and the image only becomes present once
// that stream has been read to the end.
func (d *fakeDaemon) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.begin("ImagePull", ref); err != nil {
		return nil, err
	}
	d.pulls = append(d.pulls, ref)
	progress := `{"status":"Pulling from library"}` + "\n" + `{"status":"Download complete"}` + "\n"
	return &pullStream{daemon: d, ref: ref, body: strings.NewReader(progress)}, nil
}

func (d *fakeDaemon) Close() error { return nil }

type pullStream struct {
	daemon *fakeDaemon
	ref    string
	body   io.Reader
}

func (p *pullStream) Read(b []byte) (int, error) {
	n, err := p.body.Read(b)
	if err == io.EOF {
		p.daemon.mu.Lock()
		p.daemon.images[p.ref] = true
		p.daemon.mu.Unlock()
	}
	return n, err
}

func (p *pullStream) Close() error { return nil }

// begin records a call and reports the failure a test asked that call to have.
// Callers hold d.mu.
func (d *fakeDaemon) begin(method, target string) error {
	call := method
	if target != "" {
		call += ":" + target
	}
	d.calls = append(d.calls, call)
	return d.errs[method]
}

func notFound(kind, id string) error {
	return fmt.Errorf("fake daemon: no such %s: %s: %w", kind, id, cerrdefs.ErrNotFound)
}

// --- test-side helpers ---

func (d *fakeDaemon) addImage(refs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ref := range refs {
		d.images[ref] = true
	}
}

// addContainer seeds a container as if an earlier process had left it behind.
func (d *fakeDaemon) addContainer(name, img string, running bool, labels map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.images[img] = true
	d.containers[name] = &fakeContainer{
		image:   img,
		running: running,
		config:  &container.Config{Image: img, Labels: labels},
	}
}

func (d *fakeDaemon) script(cmdline string, run fakeRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[cmdline] = run
}

func (d *fakeDaemon) failOn(method string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errs[method] = err
}

// state reports whether a container exists and whether it is running, which is
// how a test tells stopped from removed.
func (d *fakeDaemon) state(name string) (exists, running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	return ok, ok && c.running
}

func (d *fakeDaemon) created(name string) *container.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.containers[name]; c != nil {
		return c.config
	}
	return nil
}

func (d *fakeDaemon) history() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDaemon) count(method string) int {
	n := 0
	for _, c := range d.history() {
		if c == method || strings.HasPrefix(c, method+":") {
			n++
		}
	}
	return n
}

func (d *fakeDaemon) lastExec() *fakeExec {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nextID == 0 {
		return nil
	}
	return d.execs[fmt.Sprintf("exec-%d", d.nextID)]
}

func (d *fakeDaemon) stopOptions() []container.StopOptions {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.stopped)
}
