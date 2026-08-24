package testutils

import (
	"testing"

	"github.com/maikdotfi/metaharness/memory"
)

// TestMemNotes runs the shared suite against the note store with no storage at
// all, which is what keeps the suite honest: anything it asserts that MemNotes
// cannot satisfy is some backend's schema leaking into the interface.
func TestMemNotes(t *testing.T) {
	RunNoteStoreSuite(t, func(t *testing.T) memory.Store {
		t.Helper()
		return &MemNotes{}
	})
}
