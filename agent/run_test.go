package agent_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

const systemPrompt = "You are a coding agent."

const helloGo = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, world\")\n}\n"

// writeFileArgs is the shape the model "sends" as tool-call input and that the
// test decodes back out of the recording tool to check what was dispatched.
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// helloWorldScenario is the common scripted conversation: the model asks to
// write main.go, then finishes with text. It returns a fresh slice each call so
// a test can tweak it (swap the file, add a turn) without affecting others.
func helloWorldScenario(t *testing.T) []fantasy.Message {
	t.Helper()
	return []fantasy.Message{
		testutils.AssistantToolCall(t, "call_1", "write_file", writeFileArgs{Path: "main.go", Content: helloGo}),
		testutils.AssistantText("Done — wrote a hello world program to main.go."),
	}
}

// TestRunWritesHelloWorld drives the whole agent loop with a fake model: the
// model asks for a write_file tool call, the loop dispatches it, feeds the
// result back, and the model then replies with plain text to finish.
func TestRunWritesHelloWorld(t *testing.T) {
	mdl := &testutils.ScriptedModel{Replies: helloWorldScenario(t)}
	tool := &testutils.RecordingTool{Name: "write_file", Result: agent.ToolResult{Content: "wrote main.go"}}

	a := agent.New(systemPrompt,
		agent.WithModel(mdl),
		agent.WithStore(&testutils.MemStore{}),
		agent.WithSandbox(testutils.NopFactory{}),
		agent.WithTools(tool),
	)

	sess := testutils.UserSession("t1", "fake-model", "Write a hello world program in Go.")

	got := testutils.RunToCompletion(t, a, sess)

	want := []agent.EventType{agent.EventAssistant, agent.EventToolResult, agent.EventAssistant, agent.EventDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	// The loop dispatched our tool call exactly once, with the model's args.
	if len(tool.Inputs) != 1 {
		t.Fatalf("tool called %d times, want 1", len(tool.Inputs))
	}
	var gotArgs writeFileArgs
	if err := json.Unmarshal(tool.Inputs[0], &gotArgs); err != nil {
		t.Fatalf("tool input not valid JSON: %v", err)
	}
	if gotArgs.Path != "main.go" || gotArgs.Content != helloGo {
		t.Fatalf("tool args = %+v, want main.go with the hello world program", gotArgs)
	}

	if sess.Status != agent.StatusCompleted {
		t.Fatalf("session status = %q, want %q", sess.Status, agent.StatusCompleted)
	}
	if len(mdl.Calls) != 2 {
		t.Fatalf("model called %d times, want 2", len(mdl.Calls))
	}
	if sys := mdl.Calls[0].System; sys != systemPrompt {
		t.Fatalf("system prompt = %q, want it forwarded to the model", sys)
	}
	if tools := mdl.Calls[0].Tools; len(tools) != 1 || tools[0].Name != "write_file" {
		t.Fatalf("tool defs = %+v, want the write_file tool forwarded", tools)
	}
}
