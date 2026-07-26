//go:build docker

// These tests drive a real Docker daemon and are excluded from `make test`. Run
// them with `make test-docker`. METAHARNESS_TEST_IMAGE overrides the image, which
// must provide real bash — the file tools shell out through it.
package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/moby/moby/client"

	"github.com/maikdotfi/metaharness/agent"
)

const testSandboxName = "metaharness-itest"

func dockerImage() string {
	if img := os.Getenv("METAHARNESS_TEST_IMAGE"); img != "" {
		return img
	}
	return "bash:5"
}

func requireDocker(t *testing.T) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, err := cli.Ping(t.Context(), client.PingOptions{}); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}
}

// TestDockerIntegrationEphemeralSandbox proves the real thing behaves like the
// fake-daemon tests say: commands run in the container, exit codes come back as
// results, stderr comes back whichever way the command exits, the bind mount is
// the workdir, and Close takes the container away.
func TestDockerIntegrationEphemeralSandbox(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	mount := t.TempDir()
	f := agent.DockerFactory{Image: dockerImage(), Mount: mount, Dir: "/work"}

	box, err := f.Acquire(ctx, agent.SandboxSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = box.Close() })

	res, err := box.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 0 {
		t.Errorf("ExecResult = %+v, want stdout \"hi\\n\" and exit 0", res)
	}

	// A command that writes to stderr and succeeds: git, npm and every compiler
	// warning do this, and the model has to see it. It is exactly what the old
	// CLI-shelling backend threw away.
	res, err = box.Exec(ctx, agent.Command{
		Cmd: "bash", Args: []string{"-c", "echo out; echo warning >&2"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "out\n" || res.Stderr != "warning\n" {
		t.Errorf("ExecResult = %+v, want exit 0 with stdout and stderr kept apart", res)
	}

	res, err = box.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err != nil {
		t.Fatalf("a failing command must not be an infra error: %v", err)
	}
	if res.ExitCode != 3 || res.Stderr != "boom\n" {
		t.Errorf("ExecResult = %+v, want exit 3 with stderr captured", res)
	}

	// The mount is the working directory, so a relative write lands on the host.
	if _, err := box.Exec(ctx, agent.Command{
		Cmd: "bash", Args: []string{"-c", "echo written > out.txt"},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mount, "out.txt"))
	if err != nil {
		t.Fatalf("expected out.txt on the host: %v", err)
	}
	if string(got) != "written\n" {
		t.Errorf("out.txt = %q, want %q", got, "written\n")
	}

	if err := box.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := box.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", "true"}}); err == nil {
		t.Error("the throwaway container survived Close")
	}
}

// TestDockerIntegrationDurableSandboxSurvivesSleep is the whole durability claim
// against a real daemon: a named sandbox keeps its filesystem across sleep, is
// adopted by name rather than recreated, and only Destroy removes it.
func TestDockerIntegrationDurableSandboxSurvivesSleep(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	f := agent.DockerFactory{Image: dockerImage(), Dir: "/work"}
	spec := agent.SandboxSpec{Name: testSandboxName, Image: dockerImage(), Durable: true}

	_ = f.Destroy(ctx, testSandboxName) // a leftover from a failed run
	t.Cleanup(func() { _ = f.Destroy(ctx, testSandboxName) })

	box, err := f.Acquire(ctx, spec)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := box.Exec(ctx, agent.Command{
		Cmd: "bash", Args: []string{"-c", "mkdir -p /state && echo remembered > /state/note"},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	sleeper, ok := box.(agent.Sleeper)
	if !ok {
		t.Fatal("a docker sandbox must implement agent.Sleeper")
	}
	if err := sleeper.Sleep(ctx); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if err := box.Close(); err != nil { // detach, not destroy
		t.Fatalf("Close: %v", err)
	}

	// A sleeping sandbox has no compute, so a command cannot reach it — and that
	// has to be an infra error the registry can act on by waking it, never a
	// non-zero exit code the model would read as a failed command.
	if res, err := box.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", "true"}}); err == nil {
		t.Errorf("exec on a stopped container returned %+v, want an error", res)
	}

	infos, err := f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	i := slices.IndexFunc(infos, func(info agent.SandboxInfo) bool { return info.Name == testSandboxName })
	if i < 0 {
		t.Fatalf("the sleeping sandbox is gone; List = %+v", infos)
	}
	if infos[i].State != agent.SandboxAsleep || !infos[i].Durable {
		t.Errorf("listed sandbox = %+v, want a durable, asleep one", infos[i])
	}
	if infos[i].Image != dockerImage() {
		t.Errorf("listed sandbox = %+v, want the image it actually runs (%s)", infos[i], dockerImage())
	}

	// Acquiring again adopts and starts the same container, filesystem intact.
	again, err := f.Acquire(ctx, spec)
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	res, err := again.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", "cat /state/note"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "remembered\n" {
		t.Errorf("the filesystem did not survive sleep: %+v", res)
	}

	if err := f.Destroy(ctx, testSandboxName); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	infos, err = f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if slices.ContainsFunc(infos, func(info agent.SandboxInfo) bool { return info.Name == testSandboxName }) {
		t.Errorf("the sandbox survived Destroy; List = %+v", infos)
	}
}
