package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// storedSession saves one session with a transcript to store and returns its id.
func storedSession(t *testing.T, store *testutils.MemStore, id, sandboxName string) string {
	t.Helper()
	sess := testutils.UserSession(id, "fake-model", &testutils.FakeSandbox{SandboxName: sandboxName}, "hi")
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("saving %s: %v", id, err)
	}
	return id
}

// TestResumeBringsBackASessionReadyToRun is the three steps every persisting
// application used to write for itself: load the transcript, open the sandbox it
// recorded, bind the two together. A session that comes back unbound has nowhere
// to run, and that is the mistake worth making impossible.
func TestResumeBringsBackASessionReadyToRun(t *testing.T) {
	ctx := context.Background()
	store := &testutils.MemStore{}
	storedSession(t, store, "s1", "work")
	a := agent.New("test system prompt", agent.WithStore(store))

	sessions := a.Sessions(&testutils.FakeSandboxes{})
	if sessions == nil {
		t.Fatal("an agent storing to a listing store offered no sessions")
	}
	got, err := sessions.Resume(ctx, "s1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if got.ID != "s1" {
		t.Errorf("resumed %q, want %q", got.ID, "s1")
	}
	if len(got.Messages) != 1 {
		t.Errorf("resumed transcript has %d messages, want the 1 it was saved with", len(got.Messages))
	}
	box := got.Sandbox()
	if box == nil {
		t.Fatal("the resumed session has no live sandbox, so it cannot run")
	}
	if box.Name() != "work" {
		t.Errorf("resumed into sandbox %q, want the recorded %q", box.Name(), "work")
	}
}

// TestResumeOfAnUnknownSessionSaysSo keeps ErrNotFound reachable through the
// resume path: a bridge tells a user "no such session" apart from "storage
// broke" by testing for it.
func TestResumeOfAnUnknownSessionSaysSo(t *testing.T) {
	a := agent.New("test system prompt", agent.WithStore(&testutils.MemStore{}))

	_, err := a.Sessions(&testutils.FakeSandboxes{}).Resume(context.Background(), "nope")

	if !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("Resume of an unknown id = %v, want agent.ErrNotFound", err)
	}
}

// TestResumeWithNowhereToRunHandsBackNothing pins the half-resume: when the
// sandbox cannot be opened the caller gets an error and no session, so whatever
// it is running now stays untouched.
func TestResumeWithNowhereToRunHandsBackNothing(t *testing.T) {
	ctx := context.Background()
	store := &testutils.MemStore{}
	storedSession(t, store, "s1", "work")
	a := agent.New("test system prompt", agent.WithStore(store))
	unreachable := errors.New("the backend is not there")

	got, err := a.Sessions(&testutils.FakeSandboxes{OpenErr: unreachable}).Resume(ctx, "s1")

	if err == nil {
		t.Fatal("resuming with no sandbox to bind reported success")
	}
	if !errors.Is(err, unreachable) {
		t.Errorf("Resume error = %v, want it to carry %v", err, unreachable)
	}
	if got != nil {
		t.Errorf("a failed resume handed back session %q; a caller must keep its own", got.ID)
	}
}

func TestSessionsListWhatTheStoreRetains(t *testing.T) {
	ctx := context.Background()
	store := &testutils.MemStore{}
	storedSession(t, store, "s1", "work")
	storedSession(t, store, "s2", "work")
	a := agent.New("test system prompt", agent.WithStore(store))

	infos, err := a.Sessions(&testutils.FakeSandboxes{}).List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	ids := make(map[string]bool, len(infos))
	for _, info := range infos {
		ids[info.ID] = true
	}
	if !ids["s1"] || !ids["s2"] {
		t.Errorf("List returned %v, want both stored sessions", ids)
	}
}

// TestAnAgentThatRetainsNothingOffersNoSessions is what lets a bridge decide
// whether the commands over stored history exist at all: nil means nil, not a
// value that answers every call with an empty list.
func TestAnAgentThatRetainsNothingOffersNoSessions(t *testing.T) {
	a := agent.New("test system prompt") // the default store discards

	if got := a.Sessions(&testutils.FakeSandboxes{}); got != nil {
		t.Fatalf("Sessions() = %#v, want nil so a caller can tell there is no history", got)
	}
}

// TestSessionsWithNowhereToOpenSandboxesOffersNothing is the same nil for the
// same reason: history nothing can be resumed into is not history a caller can
// offer, and saying so is better than handing back a value that resumes as far as
// the sandbox and then panics.
func TestSessionsWithNowhereToOpenSandboxesOffersNothing(t *testing.T) {
	store := &testutils.MemStore{}
	storedSession(t, store, "s1", "work")
	a := agent.New("test system prompt", agent.WithStore(store))

	if got := a.Sessions(nil); got != nil {
		t.Fatalf("Sessions(nil) = %#v, want nil: there is nowhere to bind a resumed session", got)
	}
}
