package docker

import (
	"slices"
	"testing"

	"github.com/maikdotfi/metaharness/sandbox"
)

// TestImportingThisPackageRegistersTheKind is the whole contract of the blank
// import: an application that has linked this package in can ask for "docker" by
// name and get a working backend, without naming this package anywhere else.
//
// It needs no daemon — connecting is lazy — which is also what makes it a fair
// test of the wiring rather than of the host.
func TestImportingThisPackageRegistersTheKind(t *testing.T) {
	if kinds := sandbox.Backends(); !slices.Contains(kinds, Kind) {
		t.Fatalf("sandbox.Backends() = %v, want it to contain %q", kinds, Kind)
	}

	backend, err := sandbox.NewBackend(Kind, sandbox.Config{})
	if err != nil {
		t.Fatalf("sandbox.NewBackend(%q): %v", Kind, err)
	}
	t.Cleanup(func() { backend.Close() })

	if _, ok := backend.(*Backend); !ok {
		t.Errorf("sandbox.NewBackend(%q) returned %T, want *docker.Backend", Kind, backend)
	}
}

// TestTheKindIgnoresRoot pins that a host directory means nothing here: a
// sandbox is a container's own writable layer, and commands run in the
// in-container workdir whatever an application put in Config.
func TestTheKindIgnoresRoot(t *testing.T) {
	backend, err := sandbox.NewBackend(Kind, sandbox.Config{Root: "/tmp/somewhere"})
	if err != nil {
		t.Fatalf("sandbox.NewBackend(%q): %v", Kind, err)
	}
	t.Cleanup(func() { backend.Close() })

	if got := backend.(*Backend).workdir; got != DefaultWorkdir {
		t.Errorf("workdir = %q, want %q", got, DefaultWorkdir)
	}
}
