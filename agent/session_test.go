package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// execAgent wires an agent whose single tool runs one command in whatever
// sandbox the session it is given is bound to.
func execAgent(cmd string) *agent.Agent {
	return agent.New(systemPrompt,
		agent.WithModel(&testutils.ToolThenText{ToolName: "exec", Cmd: cmd}),
		agent.WithTools(&testutils.ExecTool{ToolName: "exec"}),
	)
}

// TestNewSessionBindsASandbox pins what a session is: a task transcript plus the
// one sandbox it runs in, named by the sandbox itself.
func TestNewSessionBindsASandbox(t *testing.T) {
	box := &testutils.FakeSandbox{SandboxName: "work"}

	sess := agent.NewSession("s1", "fake-model", box)

	if sess.Sandbox() != box {
		t.Errorf("Sandbox() = %v, want the sandbox the session was built with", sess.Sandbox())
	}
	if sess.SandboxName() != "work" {
		t.Errorf("SandboxName() = %q, want %q", sess.SandboxName(), "work")
	}
	if sess.Status != agent.StatusActive {
		t.Errorf("Status = %q, want %q", sess.Status, agent.StatusActive)
	}
}

// TestRunRefusesASessionWithoutASandbox checks the binding is a requirement to
// run, not something the agent fills in: an unbound session is refused up front,
// before any model call.
func TestRunRefusesASessionWithoutASandbox(t *testing.T) {
	a := execAgent("echo hi")
	sess := &agent.Session{ID: "s1", Model: "fake-model", Status: agent.StatusActive}

	if _, err := a.Run(context.Background(), sess); err == nil {
		t.Fatal("Run with an unbound session: err = nil, want an error")
	}
}

// TestToolsRunInTheSessionsSandbox follows a tool call from the model down to
// the sandbox the session is bound to.
func TestToolsRunInTheSessionsSandbox(t *testing.T) {
	a := execAgent("go build ./...")
	box := &testutils.FakeSandbox{SandboxName: "work"}
	sess := testutils.UserSession("s1", "fake-model", box, "Build it.")

	testutils.RunToCompletion(t, a, sess)

	cmds := box.Commands()
	if len(cmds) != 1 {
		t.Fatalf("sandbox %q ran %d commands, want 1", box.Name(), len(cmds))
	}
	if got := cmds[0].Args[len(cmds[0].Args)-1]; got != "go build ./..." {
		t.Errorf("command = %q, want the one the model asked for", got)
	}
}

// TestConcurrentSessionsRunInTheirOwnSandbox is the isolation claim: one agent
// serves many sessions at once, and each session's tools reach only the sandbox
// that session is bound to.
func TestConcurrentSessionsRunInTheirOwnSandbox(t *testing.T) {
	a := execAgent("make")
	boxes := map[string]*testutils.FakeSandbox{
		"alpha": {SandboxName: "alpha"},
		"beta":  {SandboxName: "beta"},
		"gamma": {SandboxName: "gamma"},
	}

	// The turns run in their own goroutines, so failures come back through a
	// channel: only the test goroutine may fail the test.
	failures := make(chan error, len(boxes))
	var wg sync.WaitGroup
	for name, box := range boxes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := testutils.UserSession("sess-"+name, "fake-model", box, "Build it.")
			events, err := a.Run(context.Background(), sess)
			if err != nil {
				failures <- err
				return
			}
			for ev := range events {
				if ev.Type == agent.EventError {
					failures <- ev.Err
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("turn failed: %v", err)
	}

	for name, box := range boxes {
		if got := len(box.Commands()); got != 1 {
			t.Errorf("sandbox %q ran %d commands, want exactly its own 1", name, got)
		}
	}
}

// TestRunLeavesTheSessionsSandboxOpen pins ownership: a finished turn does not
// take the sandbox with it — the next turn runs in the same one — and closing
// the session is what releases the handle.
func TestRunLeavesTheSessionsSandboxOpen(t *testing.T) {
	a := execAgent("make")
	box := &testutils.FakeSandbox{SandboxName: "work"}
	sess := testutils.UserSession("s1", "fake-model", box, "Build it.")

	testutils.RunToCompletion(t, a, sess)
	if box.Closes() != 0 {
		t.Fatalf("a finished turn closed the sandbox %d times, want 0", box.Closes())
	}

	sess.Messages = append(sess.Messages, fantasy.NewUserMessage("Again."))
	sess.Status = agent.StatusActive
	testutils.RunToCompletion(t, a, sess)

	if got := len(box.Commands()); got != 2 {
		t.Errorf("sandbox ran %d commands over two turns, want 2", got)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if box.Closes() != 1 {
		t.Errorf("closing the session closed the sandbox %d times, want 1", box.Closes())
	}
}

// TestRestoredSessionRunsInTheSandboxItRecorded is resumption: a session comes
// back from the store with a sandbox name and nothing live, and binding that
// name again is what makes it runnable.
func TestRestoredSessionRunsInTheSandboxItRecorded(t *testing.T) {
	ctx := context.Background()
	store := &testutils.MemStore{}

	a := execAgent("make")
	saved := testutils.UserSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "Build it.")
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if restored.SandboxName() != "work" {
		t.Fatalf("restored SandboxName() = %q, want %q", restored.SandboxName(), "work")
	}
	if restored.Sandbox() != nil {
		t.Fatal("a restored session should carry no live sandbox until it is bound")
	}

	// What an application does with the recorded name: ask the manager for that
	// sandbox again, and bind it.
	box := &testutils.FakeSandbox{SandboxName: restored.SandboxName()}
	if err := restored.Bind(box); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	testutils.RunToCompletion(t, a, restored)

	if got := len(box.Commands()); got != 1 {
		t.Errorf("restored session ran %d commands in %q, want 1", got, box.Name())
	}
}

// TestBindRefusesADifferentSandbox keeps a resumed task in the filesystem it
// started in: binding a name the session never recorded is a mistake, not a
// choice.
func TestBindRefusesADifferentSandbox(t *testing.T) {
	sess := agent.NewSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"})
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := sess.Bind(&testutils.FakeSandbox{SandboxName: "somewhere-else"})
	if !errors.Is(err, agent.ErrSandboxMismatch) {
		t.Fatalf("Bind to another sandbox: err = %v, want ErrSandboxMismatch", err)
	}
}

// TestBindRefusesAnAlreadyBoundSession stops a second handle from silently
// replacing the one the session already holds, which nobody would be left to
// close.
func TestBindRefusesAnAlreadyBoundSession(t *testing.T) {
	box := &testutils.FakeSandbox{SandboxName: "work"}
	sess := agent.NewSession("s1", "fake-model", box)

	if err := sess.Bind(&testutils.FakeSandbox{SandboxName: "work"}); err == nil {
		t.Fatal("Bind over a live sandbox: err = nil, want an error")
	}
	if sess.Sandbox() != box {
		t.Error("a refused Bind replaced the session's sandbox anyway")
	}
}
