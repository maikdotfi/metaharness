package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewIsTheWholeAssembly is the shape an application is meant to have: one
// call turns a backend kind it read from a flag into a manager it can open
// sandboxes on. Nothing in between is the application's to hold.
func TestNewIsTheWholeAssembly(t *testing.T) {
	isolateRegistry(t)
	want := newFakeBackend()
	Register("fake", func(Config) (Backend, error) { return want, nil })

	m, err := New("fake")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	box, err := m.Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { box.Close() })
	mustExec(t, box)

	if _, running := want.alive("work"); !running {
		t.Error("the manager did not run the command on the registered backend")
	}
}

// TestNewPassesBackendSettingsThrough checks the options that describe where
// sandboxes live reach the backend, since constructing it is the only chance to
// say them.
func TestNewPassesBackendSettingsThrough(t *testing.T) {
	isolateRegistry(t)
	var seen Config
	Register("fake", func(cfg Config) (Backend, error) {
		seen = cfg
		return newFakeBackend(), nil
	})

	m, err := New("fake", WithRoot("/tmp/work"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	if seen.Root != "/tmp/work" {
		t.Errorf("backend saw Root = %q, want /tmp/work", seen.Root)
	}
}

// TestNewOwnsTheBackendItConstructed checks the manager took the backend with
// it: shutting the manager down is the whole of an application's cleanup, and
// there is no second thing to close.
func TestNewOwnsTheBackendItConstructed(t *testing.T) {
	isolateRegistry(t)
	backend := newFakeBackend()
	Register("fake", func(Config) (Backend, error) { return backend, nil })

	m, err := New("fake")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := backend.closes(); got != 1 {
		t.Errorf("backend closed %d times, want 1", got)
	}
}

// TestNewReportsAnUnknownKind is the cost of choosing a backend by name: the
// mistake lands at run time, so the error has to be the whole explanation.
func TestNewReportsAnUnknownKind(t *testing.T) {
	isolateRegistry(t)
	Register("podman", func(Config) (Backend, error) { return newFakeBackend(), nil })

	_, err := New("docker")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("New with an unregistered kind: err = %v, want ErrUnknownBackend", err)
	}
	for _, want := range []string{"docker", "podman", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// TestNewReportsABackendThatWouldNotBuild checks a backend that refused to be
// constructed says why, rather than being reported as an unknown kind.
func TestNewReportsABackendThatWouldNotBuild(t *testing.T) {
	isolateRegistry(t)
	boom := errors.New("no daemon")
	Register("fake", func(Config) (Backend, error) { return nil, boom })

	if _, err := New("fake"); !errors.Is(err, boom) {
		t.Errorf("New: err = %v, want %v", err, boom)
	}
}

// TestNewDefaultsToTheLocalBackend checks the zero-configuration path over the
// real local backend: no kind named, and a working sandbox on the host.
func TestNewDefaultsToTheLocalBackend(t *testing.T) {
	root := t.TempDir()

	m, err := New("", WithRoot(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	box, err := m.Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { box.Close() })
	if _, err := box.Exec(context.Background(), echo); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "work")); err != nil {
		t.Errorf("sandbox did not land under the root: %v", err)
	}
}

// TestNewReportsAnUnusableBackendConfig checks the settings are validated where
// they are given, rather than failing later on the first command.
func TestNewReportsAnUnusableBackendConfig(t *testing.T) {
	if _, err := New(LocalKind); err == nil {
		t.Error("New(local) with no root: err = nil, want an error")
	}
}
