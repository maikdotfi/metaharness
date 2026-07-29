package testutils

import (
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

// TestMemStore runs the shared suite against the store with no storage at all.
// That is what keeps the suite honest: anything it asserts that MemStore cannot
// satisfy is an assertion about some backend's schema that has leaked into the
// interface's contract.
func TestMemStore(t *testing.T) {
	RunSessionStoreSuite(t, func(t *testing.T) agent.SessionStore {
		t.Helper()
		return &MemStore{}
	})
}
