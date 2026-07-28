package testutils

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/model"
)

// ScriptedModel is a fake ModelClient that returns a pre-baked assistant
// message per Generate call. It records the requests it received so tests can
// assert on what the loop sent (system prompt, tool defs, transcript).
type ScriptedModel struct {
	Replies []fantasy.Message
	Calls   []model.ModelRequest
	i       int
}

func (m *ScriptedModel) Generate(_ context.Context, req model.ModelRequest) (fantasy.Message, fantasy.Usage, error) {
	m.Calls = append(m.Calls, req)
	if m.i >= len(m.Replies) {
		// Exhausting the script means the loop didn't stop when we expected it
		// to; surface that instead of letting it spin forever.
		return fantasy.Message{}, fantasy.Usage{}, errors.New("ScriptedModel: no more replies")
	}
	msg := m.Replies[m.i]
	m.i++
	return msg, fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, nil
}

// ToolThenText is a fake ModelClient that asks for one tool call and then
// finishes with text, deciding which from the transcript it is handed rather
// than from a position in a script. That is what makes it usable by several
// sessions at once, where a scripted model would hand one session another's
// reply.
type ToolThenText struct {
	ToolName string
	Cmd      string // the command the tool call asks for

	mu    sync.Mutex
	calls int
}

func (m *ToolThenText) Generate(_ context.Context, req model.ModelRequest) (fantasy.Message, fantasy.Usage, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	usage := fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == fantasy.MessageRoleTool {
		return AssistantText("Ran it in " + m.Cmd + "."), usage, nil
	}
	input := `{"cmd":"` + m.Cmd + `"}`
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: m.ToolName, Input: input},
	}}, usage, nil
}

// Calls reports how many times the model was asked to generate.
func (m *ToolThenText) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
