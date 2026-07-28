// Package docker runs each named sandbox as a long-lived Docker container.
//
// A sandbox is one container, named after it and labelled as ours. Its
// filesystem is the container's own writable layer, which is why stopping a
// sandbox costs it nothing and only destroying it removes the work: a stopped
// container keeps everything, and starting it again is the same filesystem. The
// container runs a keepalive process so that commands can be executed inside it
// on demand rather than one container per command.
package docker

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/maikdotfi/metaharness/sandbox"
)

const (
	// Label marks a container as a sandbox this backend owns and carries the
	// sandbox name. Its presence is what ownership means: List reports a
	// container only if it is labelled, so containers that merely happen to
	// share the naming convention are left alone.
	//
	// It also makes sandboxes visible from the command line:
	//
	//	docker ps --all --filter label=metaharness.sandbox
	Label = "metaharness.sandbox"

	// DefaultWorkdir is where commands run inside the sandbox. It is created
	// with the container.
	DefaultWorkdir = "/workspace"

	// namePrefix keeps sandbox containers apart from everything else on the host
	// and supplies the leading character Docker requires, so a sandbox name only
	// has to be usable from its second character on.
	namePrefix = "metaharness-sandbox-"
)

// defaultKeepalive is what holds a sandbox container open between commands. It
// is `tail -f /dev/null` rather than the tidier `sleep infinity` because sleep
// only understands "infinity" in GNU coreutils: the busybox sleep in Alpine and
// friends would exit immediately, taking the sandbox with it.
func defaultKeepalive() []string { return []string{"tail", "-f", "/dev/null"} }

// usableName is what is left of Docker's container name grammar once the prefix
// has supplied the first character. A name that does not match is refused rather
// than mangled into something that does: the name is the sandbox's identity, and
// two names must never quietly become one container.
var usableName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// daemon is the part of the Docker client this backend uses. It exists so the
// backend can be tested against a daemon that is not there.
type daemon interface {
	ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error)
	ContainerCreate(ctx context.Context, config *container.Config, host *container.HostConfig, net *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, name string, opts container.StartOptions) error
	ContainerStop(ctx context.Context, name string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, name string, opts container.RemoveOptions) error
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerExecCreate(ctx context.Context, name string, opts container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	Close() error
}

// Backend is a sandbox.Backend backed by a Docker daemon.
type Backend struct {
	daemon    daemon
	workdir   string
	keepalive []string
}

var _ sandbox.Backend = (*Backend)(nil)

type Option func(*Backend)

// WithWorkdir sets the directory commands run in inside the sandbox. It is
// created with the container.
func WithWorkdir(dir string) Option {
	return func(b *Backend) { b.workdir = dir }
}

// WithKeepalive replaces the process that holds a sandbox container open. It
// must not exit on its own, and it must exist in the image: the default,
// `sleep infinity`, is not in every image.
func WithKeepalive(cmd []string) Option {
	return func(b *Backend) { b.keepalive = slices.Clone(cmd) }
}

// Kind is the name this backend answers to in sandbox.New. Importing this
// package for its side effect is what makes that name available:
//
//	import _ "github.com/maikdotfi/metaharness/sandbox/docker"
//
// which is also the only place an application that chooses its backend by name
// mentions Docker at all — and the only thing to delete to be rid of the SDK.
const Kind = "docker"

func init() {
	sandbox.Register(Kind, func(sandbox.Config) (sandbox.Backend, error) {
		// Config.Root is deliberately ignored: a sandbox here is a container's own
		// writable layer, so there is no host directory to put anything under. The
		// in-container workdir is DefaultWorkdir, which is not a host path and not
		// the same idea.
		return New()
	})
}

// New connects to the Docker daemon described by the environment (DOCKER_HOST
// and friends). Close releases that connection.
func New(opts ...Option) (*Backend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: connecting to the daemon: %w", err)
	}
	return newBackend(cli, opts...), nil
}

func newBackend(d daemon, opts ...Option) *Backend {
	b := &Backend{daemon: d, workdir: DefaultWorkdir, keepalive: defaultKeepalive()}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Close releases the connection to the daemon. It does nothing to the
// sandboxes, which is the point of them. The Manager this backend was handed to
// calls it during its own shutdown.
func (b *Backend) Close() error { return b.daemon.Close() }

// EnsureReady makes the named sandbox exist and run. A sandbox that is already
// there is started, never recreated: recreating it would take its filesystem
// with it.
func (b *Backend) EnsureReady(ctx context.Context, spec sandbox.Spec) error {
	name, err := b.containerName(spec.Name)
	if err != nil {
		return err
	}

	info, err := b.daemon.ContainerInspect(ctx, name)
	switch {
	case cerrdefs.IsNotFound(err):
		return b.create(ctx, name, spec)
	case err != nil:
		return fmt.Errorf("sandbox/docker: inspecting sandbox %q: %w", spec.Name, err)
	case info.ContainerJSONBase == nil || info.State == nil:
		return fmt.Errorf("sandbox/docker: the daemon reported no state for sandbox %q", spec.Name)
	case info.State.Running:
		return nil
	}
	return b.start(ctx, name, spec.Name)
}

// create builds the container a sandbox lives in, acquiring its image first.
func (b *Backend) create(ctx context.Context, name string, spec sandbox.Spec) error {
	if spec.Image == "" {
		return fmt.Errorf("sandbox/docker: sandbox %q does not exist and the spec has no image to create it from", spec.Name)
	}
	if err := b.ensureImage(ctx, spec.Image); err != nil {
		return err
	}

	// Nothing can ask the keepalive to leave: it is the container's PID 1 and
	// installs no signal handler, so the kernel does not deliver SIGTERM to it at
	// all. A graceful stop would therefore always be spent in full, and there is
	// nothing in a sleeping process to flush. Recording that on the container
	// makes every route to stopping it immediate, including a `docker rm -f` run
	// by hand.
	immediately := 0
	config := &container.Config{
		Image: spec.Image,
		// The keepalive replaces the image's own entrypoint and command: a
		// sandbox exists to run commands on request, and whatever the image
		// would otherwise start has nothing to do with that.
		Entrypoint:  b.keepalive,
		WorkingDir:  b.workdir,
		Labels:      map[string]string{Label: spec.Name},
		StopTimeout: &immediately,
	}
	if _, err := b.daemon.ContainerCreate(ctx, config, &container.HostConfig{}, nil, nil, name); err != nil {
		return fmt.Errorf("sandbox/docker: creating sandbox %q: %w", spec.Name, err)
	}
	return b.start(ctx, name, spec.Name)
}

func (b *Backend) start(ctx context.Context, name, sandboxName string) error {
	if err := b.daemon.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return fmt.Errorf("sandbox/docker: starting sandbox %q: %w", sandboxName, err)
	}
	return nil
}

// ensureImage makes the image available locally, pulling it only when it is not.
func (b *Backend) ensureImage(ctx context.Context, ref string) error {
	_, err := b.daemon.ImageInspect(ctx, ref)
	switch {
	case err == nil:
		return nil
	case !cerrdefs.IsNotFound(err):
		return fmt.Errorf("sandbox/docker: inspecting image %q: %w", ref, err)
	}

	body, err := b.daemon.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("sandbox/docker: pulling image %q: %w", ref, err)
	}
	defer body.Close()

	// The pull happens as its progress stream is read, so reading that stream to
	// the end is what waits for the image to actually be there.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("sandbox/docker: pulling image %q: %w", ref, err)
	}
	return nil
}

// Stop releases the sandbox's compute and keeps its filesystem. A sandbox that
// is not there has no compute to release, so that is success.
func (b *Backend) Stop(ctx context.Context, name string) error {
	cname, err := b.containerName(name)
	if err != nil {
		return err
	}

	// Kill rather than ask, for the reason given where the container is created:
	// the keepalive cannot be signalled, so a graceful timeout is only ever spent
	// in full. Sandboxes made by an older version carry no stop timeout of their
	// own, so it is given here too.
	immediately := 0
	if err := b.daemon.ContainerStop(ctx, cname, container.StopOptions{Timeout: &immediately}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("sandbox/docker: stopping sandbox %q: %w", name, err)
	}
	return nil
}

// Destroy removes the sandbox's container and with it the filesystem. A sandbox
// that is already gone is success, so destroying twice is safe.
func (b *Backend) Destroy(ctx context.Context, name string) error {
	cname, err := b.containerName(name)
	if err != nil {
		return err
	}

	opts := container.RemoveOptions{Force: true, RemoveVolumes: true}
	if err := b.daemon.ContainerRemove(ctx, cname, opts); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("sandbox/docker: destroying sandbox %q: %w", name, err)
	}
	return nil
}

// List reports the sandboxes this backend owns, sorted by name. Ownership is the
// label: anything else on the host, however it is named, is not ours to report.
func (b *Backend) List(ctx context.Context) ([]sandbox.BackendSandbox, error) {
	summaries, err := b.daemon.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", Label)),
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: listing sandboxes: %w", err)
	}

	found := make([]sandbox.BackendSandbox, 0, len(summaries))
	for _, s := range summaries {
		name := s.Labels[Label]
		if name == "" {
			continue
		}
		state := sandbox.BackendStopped
		if s.State == container.StateRunning {
			state = sandbox.BackendRunning
		}
		found = append(found, sandbox.BackendSandbox{Name: name, Image: s.Image, State: state})
	}
	slices.SortFunc(found, func(a, b sandbox.BackendSandbox) int { return strings.Compare(a.Name, b.Name) })
	return found, nil
}

// container is the container name a sandbox name maps to.
func (b *Backend) container(name string) string { return namePrefix + name }

// containerName validates a sandbox name and maps it to its container.
func (b *Backend) containerName(name string) (string, error) {
	if !usableName.MatchString(name) {
		return "", fmt.Errorf("sandbox/docker: %q is not a usable sandbox name: it must start with a letter or digit and hold only letters, digits, '_', '.' and '-'", name)
	}
	return b.container(name), nil
}
