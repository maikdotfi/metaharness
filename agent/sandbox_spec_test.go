package agent_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

var errTestAcquire = errors.New("no sandbox for you")

// specAgent wires an agent whose only job is to finish one turn, so a test can
// look at which sandbox spec the run reached for.
func specAgent(t *testing.T, factory agent.SandboxFactory, store agent.SessionStore, opts ...agent.Option) *agent.Agent {
	t.Helper()
	base := []agent.Option{
		agent.WithModel(&testutils.ScriptedModel{Replies: []fantasy.Message{
			testutils.AssistantText("done"),
		}}),
		agent.WithStore(store),
		agent.WithSandbox(factory),
	}
	return agent.New("system", append(base, opts...)...)
}

// TestRunPrefersTheSessionSandboxSpec pins that a resumed session goes back to
// the sandbox it ran in, even when the application's default points elsewhere.
func TestRunPrefersTheSessionSandboxSpec(t *testing.T) {
	factory := &testutils.SleepyFactory{}
	a := specAgent(t, factory, &testutils.MemStore{},
		agent.WithSandboxSpec(agent.SandboxSpec{Name: "default", Image: "golang:1.26", Durable: true}))

	sess := testutils.UserSession("s1", "m", "hi")
	sess.Sandbox = agent.SandboxSpec{Name: "recorded", Image: "python:3.14", Durable: true}

	testutils.RunToCompletion(t, a, sess)

	want := agent.SandboxSpec{Name: "recorded", Image: "python:3.14", Durable: true}
	if got := factory.Specs(); len(got) != 1 || got[0] != want {
		t.Fatalf("acquired %+v, want the session's own spec %+v", got, want)
	}
}

// TestRunFallsBackToTheAgentSandboxSpec pins the application-lifetime default:
// a fresh session with no spec of its own runs in the agent's sandbox.
func TestRunFallsBackToTheAgentSandboxSpec(t *testing.T) {
	factory := &testutils.SleepyFactory{}
	spec := agent.SandboxSpec{Name: "work", Image: "golang:1.26", Durable: true}
	a := specAgent(t, factory, &testutils.MemStore{}, agent.WithSandboxSpec(spec))

	testutils.RunToCompletion(t, a, testutils.UserSession("s1", "m", "hi"))

	if got := factory.Specs(); len(got) != 1 || got[0] != spec {
		t.Fatalf("acquired %+v, want the agent's default spec %+v", got, spec)
	}
}

// TestRunWithoutAnySpecAcquiresTheZeroSpec pins that callers who never mention a
// sandbox keep working untouched: the zero spec goes through, which backends like
// LocalFactory ignore.
func TestRunWithoutAnySpecAcquiresTheZeroSpec(t *testing.T) {
	factory := &testutils.SleepyFactory{}
	a := specAgent(t, factory, &testutils.MemStore{})

	testutils.RunToCompletion(t, a, testutils.UserSession("s1", "m", "hi"))

	if got := factory.Specs(); len(got) != 1 || got[0] != (agent.SandboxSpec{}) {
		t.Fatalf("acquired %+v, want a single zero spec", got)
	}
}

// TestRunRecordsResolvedSpecInTheTranscript pins the write-back: every session
// the store sees already knows which sandbox produced it, so replaying it
// reattaches to the same one instead of creating a second.
func TestRunRecordsResolvedSpecInTheTranscript(t *testing.T) {
	store := &testutils.MemStore{}
	spec := agent.SandboxSpec{Name: "work", Image: "golang:1.26", Durable: true}
	a := specAgent(t, &testutils.SleepyFactory{}, store, agent.WithSandboxSpec(spec))

	sess := testutils.UserSession("s1", "m", "hi")
	testutils.RunToCompletion(t, a, sess)

	if len(store.Snapshots) == 0 {
		t.Fatal("the store received nothing to inspect")
	}
	if got := store.Snapshots[0].Sandbox; got != spec {
		t.Errorf("first saved session's sandbox = %+v, want the resolved spec %+v", got, spec)
	}
	if sess.Sandbox != spec {
		t.Errorf("session sandbox = %+v, want the resolved spec %+v", sess.Sandbox, spec)
	}
}

// TestRunRecordsResolvedSpecEvenWhenAcquireFails pins that a run which never got
// a sandbox still records which one it was trying to use — otherwise the failed
// transcript says nothing about where it went wrong.
func TestRunRecordsResolvedSpecEvenWhenAcquireFails(t *testing.T) {
	store := &testutils.MemStore{}
	spec := agent.SandboxSpec{Name: "work", Durable: true}
	factory := &testutils.SleepyFactory{Err: errTestAcquire}
	a := specAgent(t, factory, store, agent.WithSandboxSpec(spec))

	sess := testutils.UserSession("s1", "m", "hi")
	events, err := a.Run(t.Context(), sess)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var failed bool
	for ev := range events {
		if ev.Type == agent.EventError {
			failed = true
		}
	}
	if !failed {
		t.Fatal("expected a failing run when the sandbox cannot be acquired")
	}
	if len(store.Snapshots) == 0 || store.Snapshots[0].Sandbox != spec {
		t.Errorf("saved sessions = %+v, want the attempted spec %+v recorded", store.Snapshots, spec)
	}
}

// TestSandboxSpecDecodesTranscriptsWrittenBeforeDurability pins the compatibility
// promise of adding fields to a persisted type: a session stored when a sandbox
// was only an image still loads, as an ephemeral sandbox.
func TestSandboxSpecDecodesTranscriptsWrittenBeforeDurability(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"kind":"meta","meta":{"id":"old","model":"m","status":"active",` +
		`"usage":{},"sandbox":{"image":"golang:1.26"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := agent.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	sess, err := store.Load(t.Context(), "old")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := (agent.SandboxSpec{Image: "golang:1.26"}); sess.Sandbox != want {
		t.Errorf("sandbox = %+v, want %+v", sess.Sandbox, want)
	}
}

// TestSandboxForResolvesWithoutRunning pins the resolution rule as something a
// caller can ask about — the bridge's /status needs the answer before any run
// has happened.
func TestSandboxForResolvesWithoutRunning(t *testing.T) {
	dflt := agent.SandboxSpec{Name: "work", Durable: true}
	a := agent.New("system", agent.WithSandboxSpec(dflt))

	if got := a.SandboxFor(testutils.UserSession("s1", "m", "hi")); got != dflt {
		t.Errorf("SandboxFor(fresh session) = %+v, want the agent default %+v", got, dflt)
	}

	sess := testutils.UserSession("s2", "m", "hi")
	sess.Sandbox = agent.SandboxSpec{Name: "other", Durable: true}
	if got := a.SandboxFor(sess); got != sess.Sandbox {
		t.Errorf("SandboxFor(session with a spec) = %+v, want %+v", got, sess.Sandbox)
	}
	if got := a.SandboxFor(nil); got != dflt {
		t.Errorf("SandboxFor(nil) = %+v, want the agent default %+v", got, dflt)
	}
}
