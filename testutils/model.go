package testutils

import (
	"context"
	"errors"

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
