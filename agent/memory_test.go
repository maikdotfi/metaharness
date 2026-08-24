package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// stubMemory recalls a fixed block, counting how often it was asked, and offers
// whatever tools a test hands it.
type stubMemory struct {
	notes string
	err   error
	tools []agent.Tool

	calls atomic.Int64
}

func (m *stubMemory) Recall(context.Context) (string, error) {
	m.calls.Add(1)
	return m.notes, m.err
}

func (m *stubMemory) Tools() []agent.Tool { return m.tools }

const recalled = "What you remember about the person you work for.\n\n## taste\nDeep technical dives."

// memoryAgent assembles an agent whose model runs the given script, so a test
// can read back the requests the loop sent.
func memoryAgent(mem agent.Memory, replies ...fantasy.Message) (*agent.Agent, *testutils.ScriptedModel) {
	mdl := &testutils.ScriptedModel{Replies: replies}
	opts := []agent.Option{
		agent.WithModel(mdl),
		agent.WithStore(&testutils.MemStore{}),
		agent.WithTools(&testutils.RecordingTool{Name: "write_file"}),
	}
	if mem != nil {
		opts = append(opts, agent.WithMemory(mem))
	}
	return agent.New(systemPrompt, opts...), mdl
}

func memorySession() *agent.Session {
	return testutils.UserSession("m1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "hello")
}

// TestRecallOncePerTurn pins recall as a per-turn thing: a turn that makes three
// model calls asks once, and every call carries the same system prompt — so a
// remember lands on the next turn and the prompt cache is not thrown away
// mid-turn.
func TestRecallOncePerTurn(t *testing.T) {
	mem := &stubMemory{notes: recalled}
	a, mdl := memoryAgent(mem,
		testutils.AssistantToolCall(t, "call_1", "write_file", writeFileArgs{Path: "a.go"}),
		testutils.AssistantToolCall(t, "call_2", "write_file", writeFileArgs{Path: "b.go"}),
		testutils.AssistantText("Done."),
	)

	testutils.RunToCompletion(t, a, memorySession())

	if got := mem.calls.Load(); got != 1 {
		t.Errorf("Recall called %d times, want 1", got)
	}
	if len(mdl.Calls) != 3 {
		t.Fatalf("model called %d times, want 3", len(mdl.Calls))
	}
	for i, call := range mdl.Calls {
		if call.System != mdl.Calls[0].System {
			t.Fatalf("model call %d system prompt = %q, want the same as the first call", i, call.System)
		}
	}
}

// TestRecalledNotesFollowTheApplicationPrompt pins where the notes land: the
// application's prompt is still what the model reads first.
func TestRecalledNotesFollowTheApplicationPrompt(t *testing.T) {
	a, mdl := memoryAgent(&stubMemory{notes: recalled}, testutils.AssistantText("Done."))

	testutils.RunToCompletion(t, a, memorySession())

	sys := mdl.Calls[0].System
	if !strings.HasPrefix(sys, systemPrompt) {
		t.Errorf("system prompt = %q, want it to start with the application's prompt", sys)
	}
	if !strings.HasSuffix(sys, recalled) {
		t.Errorf("system prompt = %q, want it to end with the recalled notes", sys)
	}
}

// TestNothingRecalledAddsNothing is the common case: an agent that remembers
// nothing yet sends exactly the prompt it was built with — no empty block, no
// paragraph explaining an absence.
func TestNothingRecalledAddsNothing(t *testing.T) {
	a, mdl := memoryAgent(&stubMemory{}, testutils.AssistantText("Done."))

	testutils.RunToCompletion(t, a, memorySession())

	if got := mdl.Calls[0].System; got != systemPrompt {
		t.Errorf("system prompt = %q, want %q", got, systemPrompt)
	}
}

// TestRecallErrorFailsTheTurn: silent amnesia is the worse failure, so a memory
// that cannot be read stops the turn before the model is asked anything.
func TestRecallErrorFailsTheTurn(t *testing.T) {
	wantErr := errors.New("database is gone")
	a, mdl := memoryAgent(&stubMemory{err: wantErr}, testutils.AssistantText("Done."))

	events, err := a.Run(context.Background(), memorySession())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
	if events != nil {
		t.Errorf("Run() returned a stream alongside its error")
	}
	if len(mdl.Calls) != 0 {
		t.Errorf("model called %d times, want 0", len(mdl.Calls))
	}
}

// TestMemoryToolsAreRegistered: the application hands the memory over once, and
// the memory's own tools reach the model without a second line of wiring.
func TestMemoryToolsAreRegistered(t *testing.T) {
	remember := &testutils.RecordingTool{Name: "remember", Result: agent.ToolResult{Content: "noted"}}
	mem := &stubMemory{tools: []agent.Tool{remember}}
	a, mdl := memoryAgent(mem,
		testutils.AssistantToolCall(t, "call_1", "remember", map[string]string{"topic": "taste"}),
		testutils.AssistantText("Noted."),
	)

	testutils.RunToCompletion(t, a, memorySession())

	var offered []string
	for _, def := range mdl.Calls[0].Tools {
		offered = append(offered, def.Name)
	}
	if !slices.Contains(offered, "remember") {
		t.Errorf("tools offered to the model = %v, want remember among them", offered)
	}
	if len(remember.Inputs) != 1 {
		t.Fatalf("remember called %d times, want 1", len(remember.Inputs))
	}
}

// TestMemoryToolNameCollisionPanics: a memory tool shadowing an application tool
// is a wiring mistake, and it stops the program where every other duplicate does.
func TestMemoryToolNameCollisionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New did not panic on a memory tool named like an application tool")
		}
	}()
	mem := &stubMemory{tools: []agent.Tool{&testutils.RecordingTool{Name: "write_file"}}}
	memoryAgent(mem)
}
