package agent

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Sandbox labels. They are the only bookkeeping a docker sandbox has: identity
// is the container name plus these, read back from the daemon, never from a
// store that could drift.
const (
	labelSandbox = "metaharness.sandbox"
	labelDurable = "metaharness.durable"
)

// keepalive is the container command. Exec needs a running container, and a
// container whose command has exited has nothing left to exec into.
var keepalive = []string{"sleep", "infinity"}

// DockerClient is the slice of the Docker Engine API this backend uses. It is
// the seam tests replace with a fake daemon, and it is deliberately narrow:
// depending on the whole client.APIClient would make every future method a
// method a fake has to grow.
//
// *client.Client satisfies it, and so does anything else speaking the Engine
// API — a podman socket selected through DOCKER_HOST needs no code here.
type DockerClient interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerInspect(ctx context.Context, container string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStart(ctx context.Context, container string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, container string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, container string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)

	ExecCreate(ctx context.Context, container string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
}

var _ DockerClient = (*client.Client)(nil)

// defaultClient is the daemon connection a DockerFactory uses when none was
// injected. A client is a connection pool with no per-factory configuration, so
// there is one per process, built on first use rather than at init: importing
// this package must not require a docker daemon.
//
// Where it connects is the SDK's own business (DOCKER_HOST and friends, which is
// also how a podman socket is selected). Nothing in this package reads the
// environment itself.
var defaultClient = sync.OnceValues(func() (DockerClient, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("agent: docker client: %w", err)
	}
	return c, nil
})

// DockerFactory runs sandboxes as docker containers. It is the first backend
// that can adopt, sleep, and destroy: a durable spec is adopted by container
// name if that container exists and created from the image if it does not.
type DockerFactory struct {
	// Image is the default image for specs that do not name one.
	Image string

	// Mount is an absolute host path exposed inside the container at Dir, which
	// is how the host directory LocalFactory would run in gets inside.
	Mount string

	// Dir is the working directory inside the container, e.g. "/work". Every
	// command inherits it, so it is a sandbox property rather than a per-command
	// one.
	Dir string

	// Client talks to the daemon. A nil Client uses a process-wide one
	// configured from the SDK's own environment variables.
	Client DockerClient

	// Observer, when set, is called with adoption facts this factory learns:
	// which sandboxes were adopted rather than created, and when an adopted
	// container turns out to run a different image than the spec asked for. It
	// takes the same events a Registry observer does so an application can wire
	// one function into both.
	Observer func(SandboxEvent)
}

var (
	_ SandboxFactory = DockerFactory{}
	_ Destroyer      = DockerFactory{}
	_ Lister         = DockerFactory{}
)

// Acquire adopts or creates the sandbox spec describes and returns a handle to
// it. A durable spec is looked up by name: a running container is attached to
// as-is, a stopped one is started (its filesystem is the point), and an absent
// one is created. A non-durable spec always creates a fresh throwaway container.
func (f DockerFactory) Acquire(ctx context.Context, spec SandboxSpec) (Sandbox, error) {
	if spec.Durable && spec.Name == "" {
		return nil, ErrDurableNeedsName
	}
	cli, err := f.client()
	if err != nil {
		return nil, err
	}
	if spec.Durable {
		return f.adoptOrCreate(ctx, cli, spec)
	}
	id, err := f.create(ctx, cli, spec)
	if err != nil {
		return nil, err
	}
	return &dockerBox{cli: cli, target: id}, nil
}

func (f DockerFactory) adoptOrCreate(ctx context.Context, cli DockerClient, spec SandboxSpec) (Sandbox, error) {
	box := &dockerBox{cli: cli, target: spec.Name, durable: true}

	res, err := cli.ContainerInspect(ctx, spec.Name, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		// Nothing to adopt, so create it. Only a real "no such container" counts
		// as absent: any other failure is the daemon's, and creating a second
		// container next to one that may well exist would hide it.
		if _, err := f.create(ctx, cli, spec); err != nil {
			return nil, err
		}
		return box, nil
	case err != nil:
		return nil, fmt.Errorf("agent: docker inspect %s: %w", spec.Name, err)
	}

	var image string
	if cfg := res.Container.Config; cfg != nil {
		image = cfg.Image
	}
	if st := res.Container.State; st == nil || !st.Running {
		if err := box.Wake(ctx); err != nil {
			return nil, err
		}
	}
	f.emit(SandboxEvent{Type: SandboxAdopted, Name: spec.Name, Image: image})
	if want := f.image(spec); want != "" && want != image {
		// The name is the identity, so attach anyway — but say so, because the
		// tools the agent expects may not be in there.
		f.emit(SandboxEvent{
			Type: SandboxImageMismatch, Name: spec.Name, Image: image, WantImage: want,
		})
	}
	return box, nil
}

// create starts a new container and returns the reference to address it by: the
// sandbox name for a durable container, the container id for a throwaway one.
func (f DockerFactory) create(ctx context.Context, cli DockerClient, spec SandboxSpec) (string, error) {
	image := f.image(spec)
	if image == "" {
		return "", fmt.Errorf(
			"agent: no image for sandbox %q: set SandboxSpec.Image or DockerFactory.Image", spec.Name)
	}

	labels := map[string]string{labelDurable: strconv.FormatBool(spec.Durable)}
	if spec.Name != "" {
		labels[labelSandbox] = spec.Name
	}

	opts := client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      image,
			Cmd:        keepalive,
			WorkingDir: f.Dir,
			Labels:     labels,
		},
		HostConfig: &container.HostConfig{
			// The daemon reclaims a throwaway container when it exits, even if
			// Close never runs. A durable one must outlive its handle.
			AutoRemove: !spec.Durable,
		},
	}
	if spec.Durable {
		opts.Name = spec.Name
	}
	if f.Mount != "" && f.Dir != "" {
		opts.HostConfig.Binds = []string{f.Mount + ":" + f.Dir}
	}

	created, err := cli.ContainerCreate(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("agent: docker create %s: %w", image, err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		// The created-but-unstarted container carries the durability label, so
		// Reconcile finds it: a durable one is adopted and started next time, a
		// throwaway is reaped. Removing it here would race that.
		return "", fmt.Errorf("agent: docker start %s: %w", created.ID, err)
	}
	if spec.Durable {
		return spec.Name, nil
	}
	return created.ID, nil
}

// Destroy removes a sandbox and its filesystem. It is the only thing that does.
func (f DockerFactory) Destroy(ctx context.Context, name string) error {
	cli, err := f.client()
	if err != nil {
		return err
	}
	if _, err := cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("agent: docker rm %s: %w", name, err)
	}
	return nil
}

// List reports every container this harness created, running or not.
func (f DockerFactory) List(ctx context.Context) ([]SandboxInfo, error) {
	cli, err := f.client()
	if err != nil {
		return nil, err
	}
	// Filtering on the durability label rather than the name label catches
	// throwaway containers too — the unnamed ones are exactly what reconciliation
	// needs to reap, and they carry no name label at all.
	res, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelDurable),
	})
	if err != nil {
		return nil, fmt.Errorf("agent: docker ps: %w", err)
	}

	var infos []SandboxInfo
	for _, item := range res.Items {
		state := SandboxAsleep
		if item.State == container.StateRunning {
			state = SandboxAwake
		}
		infos = append(infos, SandboxInfo{
			Name:    containerRef(item),
			State:   state,
			Durable: item.Labels[labelDurable] == "true",
			Image:   item.Image,
		})
	}
	return infos, nil
}

// containerRef is what to address a listed container by. The API reports names
// with a leading slash; the id is the fallback, so a container without one is
// still something Destroy can reach rather than something the listing drops.
func containerRef(c container.Summary) string {
	if len(c.Names) > 0 {
		if name := strings.TrimPrefix(c.Names[0], "/"); name != "" {
			return name
		}
	}
	return c.ID
}

func (f DockerFactory) image(spec SandboxSpec) string {
	if spec.Image != "" {
		return spec.Image
	}
	return f.Image
}

func (f DockerFactory) client() (DockerClient, error) {
	if f.Client != nil {
		return f.Client, nil
	}
	return defaultClient()
}

func (f DockerFactory) emit(ev SandboxEvent) {
	if f.Observer != nil {
		f.Observer(ev)
	}
}

// dockerBox is a handle on one container. Sleep and Wake are stop and start:
// compute goes away, the filesystem stays.
type dockerBox struct {
	cli     DockerClient
	target  string // container name (durable) or id (throwaway)
	durable bool
}

var (
	_ Sandbox = (*dockerBox)(nil)
	_ Sleeper = (*dockerBox)(nil)
)

// Exec runs cmd inside the container, mapping outcomes exactly as sandbox.Local
// does: a command that ran and exited non-zero is a populated ExecResult with a
// nil error, and only a command that never ran is an error.
//
// The two are structurally separate here. The guest's status comes from
// inspecting the exec, out of band from any API error, so a daemon failure —
// the container stopped, removed, unreachable — can never arrive dressed as an
// ordinary non-zero exit.
func (b *dockerBox) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	created, err := b.cli.ExecCreate(ctx, b.target, client.ExecCreateOptions{
		Cmd:          append([]string{cmd.Cmd}, cmd.Args...),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, b.execErr(err)
	}

	att, err := b.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, b.execErr(err)
	}
	defer att.Close()

	// The attached stream carries both guest streams multiplexed together, so
	// demultiplexing is the only way they come back apart — on a successful
	// command as much as on a failing one.
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil {
		return ExecResult{}, fmt.Errorf("agent: docker exec %s: reading output: %w", b.target, err)
	}

	insp, err := b.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return ExecResult{}, b.execErr(err)
	}
	return ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: insp.ExitCode,
	}, nil
}

// execErr reports a failure of the exec machinery itself, which the agent loop
// treats as fatal. A vanished container is called out by name: it is the case a
// caller can act on, by destroying or recreating the sandbox.
func (b *dockerBox) execErr(err error) error {
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("agent: sandbox %s is gone: %w", b.target, err)
	}
	return fmt.Errorf("agent: docker exec %s: %w", b.target, err)
}

// Sleep stops the container, releasing its compute and keeping its filesystem.
func (b *dockerBox) Sleep(ctx context.Context) error {
	if _, err := b.cli.ContainerStop(ctx, b.target, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("agent: docker stop %s: %w", b.target, err)
	}
	return nil
}

// Wake starts the container again, making Exec possible.
func (b *dockerBox) Wake(ctx context.Context) error {
	if _, err := b.cli.ContainerStart(ctx, b.target, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("agent: docker start %s: %w", b.target, err)
	}
	return nil
}

// Close detaches. A durable container survives it untouched; a throwaway one is
// reclaimed, which is what makes the non-durable path "gone on Close".
func (b *dockerBox) Close() error {
	if b.durable {
		return nil
	}
	opts := client.ContainerRemoveOptions{Force: true}
	if _, err := b.cli.ContainerRemove(context.Background(), b.target, opts); err != nil {
		return fmt.Errorf("agent: docker rm %s: %w", b.target, err)
	}
	return nil
}
