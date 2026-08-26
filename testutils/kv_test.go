package testutils_test

import (
	"testing"

	"github.com/maikdotfi/metaharness/agentdb"
	"github.com/maikdotfi/metaharness/testutils"
)

func TestMemKV(t *testing.T) {
	testutils.RunKVSuite(t, func(t *testing.T) agentdb.KV {
		t.Helper()
		return &testutils.MemKV{}
	})
}
