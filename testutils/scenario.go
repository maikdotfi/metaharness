package testutils

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
)

// AssistantToolCall builds an assistant turn requesting a single tool call,
// marshaling args to the JSON string the model would have produced.
func AssistantToolCall(t *testing.T, id, name string, args any) fantasy.Message {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal tool args: %v", err)
	}
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: id, ToolName: name, Input: string(input)},
	}}
}

// AssistantText builds a plain-text assistant turn, which the loop treats as done.
func AssistantText(text string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.TextPart{Text: text},
	}}
}

// UserSession returns an active Session bound to box and seeded with a single
// user turn.
func UserSession(id, modelID string, box agent.Sandbox, prompt string) *agent.Session {
	sess := agent.NewSession(id, modelID, box)
	sess.Messages = append(sess.Messages, fantasy.NewUserMessage(prompt))
	return sess
}

// RunToCompletion drives the agent to the end of its event stream and returns
// the event types emitted, failing on any error event or if the loop doesn't
// finish within a short timeout.
func RunToCompletion(t *testing.T, a *agent.Agent, sess *agent.Session) []agent.EventType {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := a.Run(ctx, sess)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got []agent.EventType
	for ev := range events {
		if ev.Type == agent.EventError {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
		got = append(got, ev.Type)
	}
	return got
}
