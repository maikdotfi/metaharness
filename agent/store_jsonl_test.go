package agent_test

import (
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestJSONLStore(t *testing.T) {
	testutils.RunSessionStoreSuite(t, func(t *testing.T) agent.SessionStore {
		t.Helper()
		store, err := agent.NewJSONLStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewJSONLStore() error = %v", err)
		}
		return store
	})
}
