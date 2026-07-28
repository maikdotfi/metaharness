package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolateRegistry gives one test an empty registry to itself, so a test can say
// exactly what is available without the real registrations — local, and docker
// wherever it is imported — being in the answer. The real ones are put back on
// cleanup.
func isolateRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = map[string]func(Config) (Backend, error){}
	registryMu.Unlock()

	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

// TestNewBackendConstructsTheRegisteredKind is what the registry is for: a name
// an application read from its flags or environment becomes the backend behind
// it, without the application knowing which package implements it.
func TestNewBackendConstructsTheRegisteredKind(t *testing.T) {
	isolateRegistry(t)
	want := newFakeBackend()
	Register("fake", func(Config) (Backend, error) { return want, nil })

	got, err := NewBackend("fake", Config{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if got != want {
		t.Errorf("NewBackend returned %#v, want the registered backend", got)
	}
}

// TestNewBackendPassesTheConfigThrough checks the settings an application chose
// reach the backend, since constructing it is the only chance to say them.
func TestNewBackendPassesTheConfigThrough(t *testing.T) {
	isolateRegistry(t)
	var seen Config
	Register("fake", func(cfg Config) (Backend, error) {
		seen = cfg
		return newFakeBackend(), nil
	})

	if _, err := NewBackend("fake", Config{Root: "/tmp/work"}); err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if seen.Root != "/tmp/work" {
		t.Errorf("backend saw Root = %q, want /tmp/work", seen.Root)
	}
}

// TestNewBackendReportsAConstructionFailure checks a backend that could not be
// built says why, rather than being reported as an unknown kind.
func TestNewBackendReportsAConstructionFailure(t *testing.T) {
	isolateRegistry(t)
	boom := errors.New("no daemon")
	Register("fake", func(Config) (Backend, error) { return nil, boom })

	if _, err := NewBackend("fake", Config{}); !errors.Is(err, boom) {
		t.Errorf("NewBackend: err = %v, want %v", err, boom)
	}
}

// TestUnknownBackendSaysWhatIsAvailable is the cost of choosing a backend by
// name: the mistake lands at run time, so the error has to be the whole
// explanation. It names what was asked for, what there is, and — because a
// missing import is the likely cause rather than a typo — that importing the
// package is what adds a kind.
func TestUnknownBackendSaysWhatIsAvailable(t *testing.T) {
	isolateRegistry(t)
	Register("local", func(Config) (Backend, error) { return newFakeBackend(), nil })
	Register("podman", func(Config) (Backend, error) { return newFakeBackend(), nil })

	_, err := NewBackend("docker", Config{})
	if err == nil {
		t.Fatal("NewBackend with an unregistered kind: err = nil, want an error")
	}
	if !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("err = %v, want ErrUnknownBackend", err)
	}
	for _, want := range []string{"docker", "local", "podman", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// TestBackendsListsWhatIsRegistered is how an application tells a user what it
// can do — flag help, or an error of its own — without keeping its own list in
// step with the imports.
func TestBackendsListsWhatIsRegistered(t *testing.T) {
	isolateRegistry(t)
	Register("podman", func(Config) (Backend, error) { return newFakeBackend(), nil })
	Register("local", func(Config) (Backend, error) { return newFakeBackend(), nil })

	if got, want := Backends(), []string{"local", "podman"}; !slices.Equal(got, want) {
		t.Errorf("Backends() = %v, want %v", got, want)
	}
}

// TestRegisterRejectsADuplicateKind checks two packages claiming one name is a
// startup panic and not a coin flip over which backend an application gets.
func TestRegisterRejectsADuplicateKind(t *testing.T) {
	isolateRegistry(t)
	Register("fake", func(Config) (Backend, error) { return newFakeBackend(), nil })

	defer func() {
		if recover() == nil {
			t.Error("registering a kind twice did not panic")
		}
	}()
	Register("fake", func(Config) (Backend, error) { return newFakeBackend(), nil })
}

// TestRegisterRejectsAnUnusableRegistration checks a kind nobody could ask for,
// and a kind with nothing behind it, fail where the mistake is.
func TestRegisterRejectsAnUnusableRegistration(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		open func(Config) (Backend, error)
	}{
		{"empty kind", "", func(Config) (Backend, error) { return newFakeBackend(), nil }},
		{"nil constructor", "fake", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRegistry(t)
			defer func() {
				if recover() == nil {
					t.Errorf("Register with %s did not panic", tc.name)
				}
			}()
			Register(tc.kind, tc.open)
		})
	}
}

// TestLocalIsRegisteredOutOfTheBox checks the development backend needs no
// import of its own, and that Config.Root is what decides where its sandboxes
// land — the one setting it cannot work without.
func TestLocalIsRegisteredOutOfTheBox(t *testing.T) {
	root := t.TempDir()

	backend, err := NewBackend("local", Config{Root: root})
	if err != nil {
		t.Fatalf("NewBackend(local): %v", err)
	}
	t.Cleanup(func() { backend.Close() })

	if err := backend.EnsureReady(context.Background(), Spec{Name: "work"}); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work")); err != nil {
		t.Errorf("sandbox did not land under Config.Root: %v", err)
	}
}

// TestLocalNeedsARoot checks the one thing the local backend cannot be asked for
// without: with no Root it would create sandboxes wherever the process happens
// to be running, which is somebody's source tree often enough to be worth an
// error instead.
func TestLocalNeedsARoot(t *testing.T) {
	if _, err := NewBackend("local", Config{}); err == nil {
		t.Error("NewBackend(local) with no Root: err = nil, want an error")
	}
}
