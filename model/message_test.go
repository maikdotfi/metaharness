package model

import (
	"errors"
	"testing"

	"charm.land/fantasy"
)

func TestMessageViews(t *testing.T) {
	wantErr := errors.New("failed")
	message := fantasy.Message{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "hello"},
			fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "read_file", Input: `{"path":"x"}`},
			fantasy.ToolResultPart{
				ToolCallID: "call-1",
				Output:     fantasy.ToolResultOutputContentText{Text: "contents"},
			},
			fantasy.ToolResultPart{
				ToolCallID: "call-2",
				Output:     fantasy.ToolResultOutputContentError{Error: wantErr},
			},
		},
	}

	texts := TextParts(&message)
	if len(texts) != 1 || texts[0].Text != "hello" {
		t.Fatalf("TextParts() = %#v", texts)
	}

	calls := ToolCalls(&message)
	if len(calls) != 1 || calls[0].ID != "call-1" || calls[0].Name != "read_file" || calls[0].Input != `{"path":"x"}` {
		t.Fatalf("ToolCalls() = %#v", calls)
	}

	results := ToolResults(&message)
	if len(results) != 2 {
		t.Fatalf("ToolResults() = %#v", results)
	}
	if results[0].ID != "call-1" || results[0].Text != "contents" || results[0].Error != nil {
		t.Fatalf("successful ToolResult = %#v", results[0])
	}
	if results[1].ID != "call-2" || !errors.Is(results[1].Error, wantErr) {
		t.Fatalf("failed ToolResult = %#v", results[1])
	}
}

func TestMessageViewsAcceptNil(t *testing.T) {
	if TextParts(nil) != nil || ToolCalls(nil) != nil || ToolResults(nil) != nil {
		t.Fatal("nil message should produce nil views")
	}
}

func TestNewToolResultMessage(t *testing.T) {
	wantErr := errors.New("failed")
	message := NewToolResultMessage(
		ToolResult{ID: "call-1", Text: "contents"},
		ToolResult{ID: "call-2", Error: wantErr},
	)

	results := ToolResults(&message)
	if len(results) != 2 {
		t.Fatalf("ToolResults() = %#v", results)
	}
	if results[0].ID != "call-1" || results[0].Text != "contents" || results[0].Error != nil {
		t.Fatalf("successful ToolResult = %#v", results[0])
	}
	if results[1].ID != "call-2" || !errors.Is(results[1].Error, wantErr) {
		t.Fatalf("failed ToolResult = %#v", results[1])
	}
}
