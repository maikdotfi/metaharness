package agent_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/maikdotfi/metaharness/agent"
)

// fakeDocker is a tiny in-memory Docker daemon standing in for the real one. It
// keeps container state rather than a script of canned answers, so tests say
// "there is a stopped container called work" and then assert on what the daemon
// looks like afterwards — which is the behaviour, not the call sequence.
type fakeDocker struct {
	containers []*fakeContainer // in creation order, which is List order
	execs      map[string]*fakeExec
	nextID     int

	// run answers every guest command. A nil run means "prints nothing, exits 0".
	run func(cmd []string) fakeRun

	// fail injects a daemon-side failure for a client method, e.g.
	// fail["ContainerInspect"] = errors.New("daemon unreachable").
	fail map[string]error

	// calls records every client method reached, in order, so a test can assert
	// that something never touched the daemon at all.
	calls []string
}

// fakeContainer is one container's state: what it was created with and whether
// it currently has compute.
type fakeContainer struct {
	id         string
	name       string
	image      string
	cmd        []string
	workDir    string
	binds      []string
	labels     map[string]string
	autoRemove bool
	running    bool
}

// fakeExec is one created exec, remembered so a test can see what was run where.
type fakeExec struct {
	container string
	cmd       []string
	workDir   string
	run       fakeRun
}

// fakeRun is what a scripted guest command does: the stream frames it writes, in
// the order the daemon multiplexes them, and the status it exits with.
type fakeRun struct {
	Frames []fakeFrame
	Exit   int
}

// fakeFrame is one write by the guest to one of its two output streams.
type fakeFrame struct {
	Err  bool // stderr rather than stdout
	Text string
}

func outFrame(s string) fakeFrame { return fakeFrame{Text: s} }
func errFrame(s string) fakeFrame { return fakeFrame{Err: true, Text: s} }

var _ agent.DockerClient = (*fakeDocker)(nil)

// answer scripts the same result for every guest command.
func (d *fakeDocker) answer(r fakeRun) { d.run = func([]string) fakeRun { return r } }

// seed adds a container that already exists on the daemon, labelled the way this
// harness labels the ones it creates.
func (d *fakeDocker) seed(c *fakeContainer) *fakeContainer {
	if c.labels == nil {
		c.labels = map[string]string{}
	}
	if c.id == "" {
		c.id = d.newID()
	}
	d.containers = append(d.containers, c)
	return c
}

// seedSandbox adds a durable sandbox container as this harness would have left it.
func (d *fakeDocker) seedSandbox(name, image string, running bool) *fakeContainer {
	return d.seed(&fakeContainer{
		name: name, image: image, running: running,
		labels: map[string]string{"metaharness.sandbox": name, "metaharness.durable": "true"},
	})
}

// find resolves a container by name or id, the two references docker accepts.
func (d *fakeDocker) find(ref string) *fakeContainer {
	for _, c := range d.containers {
		if c.id == ref || c.name == ref {
			return c
		}
	}
	return nil
}

// names lists the containers that exist, in creation order.
func (d *fakeDocker) names() []string {
	out := make([]string, 0, len(d.containers))
	for _, c := range d.containers {
		out = append(out, c.name)
	}
	return out
}

// lastExec is the most recent exec created, or nil if none was.
func (d *fakeDocker) lastExec() *fakeExec {
	var last *fakeExec
	best := -1
	for id, e := range d.execs {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "exec")); err == nil && n > best {
			best, last = n, e
		}
	}
	return last
}

func (d *fakeDocker) newID() string {
	d.nextID++
	return fmt.Sprintf("ctr%d", d.nextID)
}

// enter records a call and reports the failure injected for it, if any.
func (d *fakeDocker) enter(method string) error {
	d.calls = append(d.calls, method)
	return d.fail[method]
}

func notFound(ref string) error {
	return fmt.Errorf("no such container: %s: %w", ref, cerrdefs.ErrNotFound)
}

func (d *fakeDocker) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := d.enter("ContainerCreate"); err != nil {
		return client.ContainerCreateResult{}, err
	}
	if opts.Name != "" && d.find(opts.Name) != nil {
		return client.ContainerCreateResult{}, fmt.Errorf("name %s already in use: %w", opts.Name, cerrdefs.ErrConflict)
	}
	c := &fakeContainer{
		id:         d.newID(),
		name:       opts.Name,
		image:      opts.Config.Image,
		cmd:        opts.Config.Cmd,
		workDir:    opts.Config.WorkingDir,
		labels:     opts.Config.Labels,
		binds:      opts.HostConfig.Binds,
		autoRemove: opts.HostConfig.AutoRemove,
	}
	if c.name == "" {
		// The daemon always names a container, even one nobody named.
		c.name = "generated_" + c.id
	}
	d.containers = append(d.containers, c)
	return client.ContainerCreateResult{ID: c.id}, nil
}

func (d *fakeDocker) ContainerInspect(_ context.Context, ref string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := d.enter("ContainerInspect"); err != nil {
		return client.ContainerInspectResult{}, err
	}
	c := d.find(ref)
	if c == nil {
		return client.ContainerInspectResult{}, notFound(ref)
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID:     c.id,
		Name:   "/" + c.name,
		State:  &container.State{Running: c.running},
		Config: &container.Config{Image: c.image, Labels: c.labels},
	}}, nil
}

func (d *fakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	if err := d.enter("ContainerList"); err != nil {
		return client.ContainerListResult{}, err
	}
	var items []container.Summary
	for _, c := range d.containers {
		if !opts.All && !c.running {
			continue
		}
		if !matchesFilters(c.labels, opts.Filters) {
			continue
		}
		state := container.StateExited
		if c.running {
			state = container.StateRunning
		}
		items = append(items, container.Summary{
			ID:     c.id,
			Names:  namesOf(c),
			Image:  c.image,
			Labels: c.labels,
			State:  state,
		})
	}
	return client.ContainerListResult{Items: items}, nil
}

func namesOf(c *fakeContainer) []string {
	if c.name == "" {
		return nil
	}
	return []string{"/" + c.name}
}

// matchesFilters applies the label filters docker would. Every label term has to
// match, in either the "key" (present) or "key=value" form. Any other filter term
// matches nothing, so a backend that starts filtering on something else fails
// loudly here rather than quietly listing the same containers.
func matchesFilters(labels map[string]string, f client.Filters) bool {
	for term, values := range f {
		if term != "label" {
			return false
		}
		for v := range values {
			key, want, hasValue := strings.Cut(v, "=")
			got, ok := labels[key]
			if !ok || (hasValue && got != want) {
				return false
			}
		}
	}
	return true
}

func (d *fakeDocker) ContainerStart(_ context.Context, ref string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if err := d.enter("ContainerStart"); err != nil {
		return client.ContainerStartResult{}, err
	}
	c := d.find(ref)
	if c == nil {
		return client.ContainerStartResult{}, notFound(ref)
	}
	c.running = true
	return client.ContainerStartResult{}, nil
}

func (d *fakeDocker) ContainerStop(_ context.Context, ref string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if err := d.enter("ContainerStop"); err != nil {
		return client.ContainerStopResult{}, err
	}
	c := d.find(ref)
	if c == nil {
		return client.ContainerStopResult{}, notFound(ref)
	}
	c.running = false
	return client.ContainerStopResult{}, nil
}

func (d *fakeDocker) ContainerRemove(_ context.Context, ref string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := d.enter("ContainerRemove"); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	c := d.find(ref)
	if c == nil {
		return client.ContainerRemoveResult{}, notFound(ref)
	}
	if c.running && !opts.Force {
		return client.ContainerRemoveResult{}, fmt.Errorf("container %s is running: %w", ref, cerrdefs.ErrConflict)
	}
	d.containers = slices.DeleteFunc(d.containers, func(x *fakeContainer) bool { return x == c })
	return client.ContainerRemoveResult{}, nil
}

func (d *fakeDocker) ExecCreate(_ context.Context, ref string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if err := d.enter("ExecCreate"); err != nil {
		return client.ExecCreateResult{}, err
	}
	c := d.find(ref)
	if c == nil {
		return client.ExecCreateResult{}, notFound(ref)
	}
	if !c.running {
		return client.ExecCreateResult{}, fmt.Errorf("container %s is not running: %w", ref, cerrdefs.ErrConflict)
	}
	run := fakeRun{}
	if d.run != nil {
		run = d.run(opts.Cmd)
	}
	if d.execs == nil {
		d.execs = map[string]*fakeExec{}
	}
	id := fmt.Sprintf("exec%d", len(d.execs)+1)
	d.execs[id] = &fakeExec{container: ref, cmd: opts.Cmd, workDir: opts.WorkingDir, run: run}
	return client.ExecCreateResult{ID: id}, nil
}

func (d *fakeDocker) ExecAttach(_ context.Context, execID string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if err := d.enter("ExecAttach"); err != nil {
		return client.ExecAttachResult{}, err
	}
	e := d.execs[execID]
	if e == nil {
		return client.ExecAttachResult{}, notFound(execID)
	}
	var stream []byte
	for _, f := range e.run.Frames {
		stream = append(stream, frame(f)...)
	}
	conn := &streamConn{r: strings.NewReader(string(stream))}
	return client.ExecAttachResult{
		HijackedResponse: client.NewHijackedResponse(conn, "application/vnd.docker.multiplexed-stream"),
	}, nil
}

func (d *fakeDocker) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	if err := d.enter("ExecInspect"); err != nil {
		return client.ExecInspectResult{}, err
	}
	e := d.execs[execID]
	if e == nil {
		return client.ExecInspectResult{}, notFound(execID)
	}
	return client.ExecInspectResult{ID: execID, ContainerID: e.container, ExitCode: e.run.Exit}, nil
}

// frame encodes one guest write the way the daemon multiplexes it: an eight byte
// header carrying the stream and the big-endian payload length, then the payload.
func frame(f fakeFrame) []byte {
	stream := stdcopy.Stdout
	if f.Err {
		stream = stdcopy.Stderr
	}
	buf := make([]byte, 8, 8+len(f.Text))
	buf[0] = byte(stream)
	binary.BigEndian.PutUint32(buf[4:], uint32(len(f.Text)))
	return append(buf, f.Text...)
}

// streamConn serves a fixed byte stream as the net.Conn the SDK hijacks for an
// attached exec.
type streamConn struct{ r io.Reader }

var _ net.Conn = (*streamConn)(nil)

func (c *streamConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *streamConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *streamConn) Close() error                     { return nil }
func (c *streamConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *streamConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
