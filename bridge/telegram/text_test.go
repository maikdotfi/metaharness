package telegram

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
)

func TestFinalTextJoinsTextPartsOnly(t *testing.T) {
	m := &fantasy.Message{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "thinking hard"},
			fantasy.TextPart{Text: "  hello  "},
			fantasy.ToolCallPart{ToolCallID: "1", ToolName: "bash", Input: `{"cmd":"ls"}`},
			fantasy.TextPart{Text: "world"},
		},
	}
	if got, want := finalText(m), "hello  \nworld"; got != want {
		t.Fatalf("finalText() = %q, want %q", got, want)
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  []string
	}{
		{"empty", "", 10, nil},
		{"short", "hello", 10, []string{"hello"}},
		{"exact", "helloworld", 10, []string{"helloworld"}},
		{"breaks on newline", "aaaa\nbbbbbb", 8, []string{"aaaa", "bbbbbb"}},
		{"breaks on space", "aaaa bbbbbb", 8, []string{"aaaa", "bbbbbb"}},
		{"hard cut when no separator", "aaaaaaaaaaaa", 5, []string{"aaaaa", "aaaaa", "aa"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitMessage(tc.in, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("splitMessage(%q, %d) = %#v, want %#v", tc.in, tc.limit, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("chunk %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitMessagePreservesUTF8Boundaries(t *testing.T) {
	// Multi-byte runes: splitting by bytes would corrupt these. "世界" is 3
	// bytes per rune, so a byte-based cut mid-rune would produce invalid UTF-8.
	s := strings.Repeat("世界🙂", 100)
	chunks := splitMessage(s, 7)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	var rebuilt strings.Builder
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk is not valid UTF-8: %q", c)
		}
		if utf8.RuneCountInString(c) > 7 {
			t.Fatalf("chunk exceeds rune limit: %q (%d runes)", c, utf8.RuneCountInString(c))
		}
		rebuilt.WriteString(c)
	}
	if rebuilt.String() != s {
		t.Fatal("rejoined chunks do not reconstruct the original string")
	}
}

func assistantEvent(parts ...fantasy.MessagePart) agent.Event {
	m := &fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
	return agent.Event{Type: agent.EventAssistant, Message: m}
}

func TestStepLine(t *testing.T) {
	tests := []struct {
		name         string
		ev           agent.Event
		showThinking bool
		wantOK       bool
		wantLine     string
	}{
		{
			name:     "tool call summarizes meaningful field",
			ev:       assistantEvent(fantasy.ToolCallPart{ToolCallID: "1", ToolName: "bash", Input: `{"cmd":"ls -la"}`}),
			wantOK:   true,
			wantLine: "🔧 bash: ls -la",
		},
		{
			name:     "read_file uses path",
			ev:       assistantEvent(fantasy.ToolCallPart{ToolCallID: "1", ToolName: "read_file", Input: `{"path":"agent/run.go"}`}),
			wantOK:   true,
			wantLine: "🔧 read_file: agent/run.go",
		},
		{
			name:         "reasoning without showThinking is a bare step",
			ev:           assistantEvent(fantasy.ReasoningPart{Text: "long private reasoning"}),
			showThinking: false,
			wantOK:       true,
			wantLine:     "💭 thinking…",
		},
		{
			name:         "reasoning with showThinking exposes text",
			ev:           assistantEvent(fantasy.ReasoningPart{Text: "weighing options"}),
			showThinking: true,
			wantOK:       true,
			wantLine:     "💭 weighing options",
		},
		{
			name:   "text-only assistant is not a progress step",
			ev:     assistantEvent(fantasy.TextPart{Text: "here is the answer"}),
			wantOK: false,
		},
		{
			name:     "done",
			ev:       agent.Event{Type: agent.EventDone, Message: &fantasy.Message{Role: fantasy.MessageRoleAssistant}},
			wantOK:   true,
			wantLine: "✅ done",
		},
		{
			name:     "error",
			ev:       agent.Event{Type: agent.EventError, Err: errors.New("boom")},
			wantOK:   true,
			wantLine: "❌ boom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := stepLine(tc.ev, tc.showThinking)
			if ok != tc.wantOK {
				t.Fatalf("stepLine ok = %v, want %v (line %q)", ok, tc.wantOK, line)
			}
			if tc.wantOK && line != tc.wantLine {
				t.Fatalf("stepLine line = %q, want %q", line, tc.wantLine)
			}
		})
	}
}

func TestStepLineToolResult(t *testing.T) {
	okResult := agent.Event{
		Type:    agent.EventToolResult,
		Message: &fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "1", Output: fantasy.ToolResultOutputContentText{Text: "total 0\nfile.go"}}}},
	}
	line, ok := stepLine(okResult, false)
	if !ok || !strings.HasPrefix(line, "↳ ") {
		t.Fatalf("ok tool result line = %q, ok = %v", line, ok)
	}

	errResult := agent.Event{
		Type:    agent.EventToolResult,
		Message: &fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "1", Output: fantasy.ToolResultOutputContentError{Error: errors.New("no such file")}}}},
	}
	line, ok = stepLine(errResult, false)
	if !ok || !strings.HasPrefix(line, "⚠️ ") {
		t.Fatalf("error tool result line = %q, ok = %v", line, ok)
	}
}
