package memory_test

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/memory"
	"github.com/maikdotfi/metaharness/testutils"
)

const systemPrompt = "You are a helpful assistant."

// rememberTool reaches the memory's own tool the way the agent does: the
// application never names it, so a test gets it from the memory too.
func rememberTool(t *testing.T, m agent.Memory) agent.Tool {
	t.Helper()
	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("memory offers %d tools, want 1", len(tools))
	}
	if name := tools[0].Definition().Name; name != "remember" {
		t.Fatalf("tool name = %q, want remember", name)
	}
	return tools[0]
}

func remember(t *testing.T, m agent.Memory, args map[string]any) agent.ToolResult {
	t.Helper()
	return testutils.CallTool(t, &agent.ExecCtx{}, rememberTool(t, m), args)
}

func recall(t *testing.T, m agent.Memory) string {
	t.Helper()
	got, err := m.Recall(context.Background())
	if err != nil {
		t.Fatalf("Recall() error = %v", err)
	}
	return got
}

// TestRememberANewTopic is the whole point: the model writes something down and
// the next recall carries it.
func TestRememberANewTopic(t *testing.T) {
	m := memory.SystemPrompt(&testutils.MemNotes{})

	res := remember(t, m, map[string]any{"topic": "taste", "content": "Prefer deep technical dives."})
	if res.IsError {
		t.Fatalf("remember reported an error: %s", res.Content)
	}

	if got := recall(t, m); !strings.Contains(got, "## taste\nPrefer deep technical dives.") {
		t.Errorf("Recall() = %q, want the new note in it", got)
	}
}

// TestRememberAddsALine: a second thing about the same topic joins the first
// rather than replacing it, because the model was not asked to restate what it
// already knew.
func TestRememberAddsALine(t *testing.T) {
	m := memory.SystemPrompt(&testutils.MemNotes{})

	remember(t, m, map[string]any{"topic": "taste", "content": "Prefer deep technical dives."})
	remember(t, m, map[string]any{"topic": "taste", "content": "Hard no on anything over three hours."})

	want := "## taste\nPrefer deep technical dives.\nHard no on anything over three hours."
	if got := recall(t, m); !strings.Contains(got, want) {
		t.Errorf("Recall() = %q, want it to contain %q", got, want)
	}
}

// TestRememberReplaces covers the correction: what the user now says about a
// topic stands instead of what they said before.
func TestRememberReplaces(t *testing.T) {
	m := memory.SystemPrompt(&testutils.MemNotes{})

	remember(t, m, map[string]any{"topic": "taste", "content": "Prefer deep technical dives."})
	remember(t, m, map[string]any{"topic": "taste", "content": "Interviews only.", "replace": true})

	got := recall(t, m)
	if !strings.Contains(got, "## taste\nInterviews only.") {
		t.Errorf("Recall() = %q, want the replacement", got)
	}
	if strings.Contains(got, "deep technical dives") {
		t.Errorf("Recall() = %q, want the replaced note gone", got)
	}
}

// TestRecallDoesNotDependOnStoreOrder: the rendered block is the same text for
// the same notes however the store hands them over, which is what lets a prompt
// cache hit across turns.
func TestRecallDoesNotDependOnStoreOrder(t *testing.T) {
	m := memory.SystemPrompt(&testutils.MemNotes{})
	remember(t, m, map[string]any{"topic": "audience", "content": "One listener, technical."})
	remember(t, m, map[string]any{"topic": "taste", "content": "Deep dives."})

	got := recall(t, m)
	if audience, taste := strings.Index(got, "## audience"), strings.Index(got, "## taste"); audience > taste {
		t.Errorf("Recall() = %q, want a topic order the store cannot change", got)
	}

	reverse := memory.SystemPrompt(&testutils.MemNotes{})
	remember(t, reverse, map[string]any{"topic": "taste", "content": "Deep dives."})
	remember(t, reverse, map[string]any{"topic": "audience", "content": "One listener, technical."})
	if other := recall(t, reverse); other != got {
		t.Errorf("Recall() = %q for notes written in the other order, want %q", other, got)
	}
}

// TestRecallWithNoNotesIsEmpty: the common case has no line. An agent that
// remembers nothing yet adds nothing to the prompt — no empty block, no
// paragraph explaining an absence.
func TestRecallWithNoNotesIsEmpty(t *testing.T) {
	if got := recall(t, memory.SystemPrompt(&testutils.MemNotes{})); got != "" {
		t.Errorf("Recall() = %q, want the empty string", got)
	}
}

// TestANoteSurvivesTheSession is the behaviour the whole thing exists for: what
// the model wrote down in one turn is in the system prompt of the next, in a
// session that did not exist when it was written.
func TestANoteSurvivesTheSession(t *testing.T) {
	store := &testutils.MemNotes{}
	mdl := &testutils.ScriptedModel{Replies: []fantasy.Message{
		testutils.AssistantToolCall(t, "call_1", "remember", map[string]string{
			"topic":   "taste",
			"content": "Prefer deep technical dives.",
		}),
		testutils.AssistantText("Noted."),
		testutils.AssistantText("Here is your digest."),
	}}
	a := agent.New(systemPrompt,
		agent.WithModel(mdl),
		agent.WithStore(&testutils.MemStore{}),
		agent.WithMemory(memory.SystemPrompt(store)),
	)

	box := &testutils.FakeSandbox{SandboxName: "work"}
	testutils.RunToCompletion(t, a, testutils.UserSession("first", "fake", box, "I like deep dives."))
	testutils.RunToCompletion(t, a, testutils.UserSession("second", "fake", box, "Give me today's digest."))

	// The turn that wrote the note did not see it; the next session did.
	if sys := mdl.Calls[0].System; sys != systemPrompt {
		t.Errorf("first system prompt = %q, want the application's prompt alone", sys)
	}
	last := mdl.Calls[len(mdl.Calls)-1].System
	if !strings.Contains(last, "Prefer deep technical dives.") {
		t.Errorf("last system prompt = %q, want the remembered note in it", last)
	}
}
