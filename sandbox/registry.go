package sandbox

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// ErrUnknownBackend is what asking for a kind nobody registered gets. It is
// almost always a missing import rather than a typo: a backend package is what
// registers its kind, so a kind is only available where its package is linked in.
var ErrUnknownBackend = errors.New("sandbox: unknown backend")

// Config is what a backend is handed at construction. Every field is optional,
// and a backend ignores what does not apply to it — which is the price of
// choosing a backend by name: one set of settings has to serve all of them.
//
// It is deliberately about where sandboxes live and nothing else. Applications
// do not fill it in: New assembles it from its options, and this is the type a
// backend's own constructor receives.
type Config struct {
	// Root is the host directory under which a backend that keeps sandboxes on
	// the local filesystem creates them, as set by WithRoot. A backend with
	// storage of its own — a container's own writable layer, say — ignores it.
	Root string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]func(Config) (Backend, error){}
)

// Register makes a backend kind constructible by name. A backend package calls
// it from an init function, which is what makes importing that package the whole
// act of making its kind available:
//
//	import _ "github.com/maikdotfi/metaharness/sandbox/docker"
//
// An application then names a kind to New — from a flag, or the environment — and
// never mentions the implementing package again. Dropping the import drops the
// dependency, and the kind along with it.
//
// It panics on an empty kind, a nil constructor, or a kind that is already
// taken. All three are mistakes in the wiring rather than conditions to recover
// from, and two packages quietly fighting over one name is worse than not
// starting.
func Register(kind string, open func(Config) (Backend, error)) {
	if kind == "" {
		panic("sandbox: Register needs a backend kind")
	}
	if open == nil {
		panic("sandbox: Register(" + kind + ") needs a constructor")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, taken := registry[kind]; taken {
		panic("sandbox: backend " + kind + " is registered twice")
	}
	registry[kind] = open
}

// NewBackend constructs the backend registered under kind.
//
// Applications call New instead, which does this and builds the Manager that
// owns the result in one step — a bare backend is of no use on its own. This is
// for a backend package testing its own registration, where getting the concrete
// type back is the point.
func NewBackend(kind string, cfg Config) (Backend, error) {
	registryMu.RLock()
	open, ok := registry[kind]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf(
			"%w %q: have %s (a backend is registered by importing its package)",
			ErrUnknownBackend, kind, strings.Join(Backends(), ", "),
		)
	}
	return open(cfg)
}

// Backends lists the registered kinds, sorted. It is what an application shows
// in flag help or its own errors, so the list follows the imports rather than
// being kept in step with them by hand.
func Backends() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return slices.Sorted(maps.Keys(registry))
}
