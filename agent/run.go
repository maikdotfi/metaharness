package agent

import (
	"context"
	"encoding/json"
	"errors"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/model"
)

type EventType string

const (
	EventAssistant  EventType = "assistant"
	EventToolResult EventType = "tool_result"
	EventDone       EventType = "done"
	EventError      EventType = "error"
)

type Event struct {
	Type    EventType
	Message *fantasy.Message
	Err     error
}

func (a *Agent) Run(ctx context.Context, sess *Session) (<-chan Event, error) {
	if sess == nil {
		return nil, errors.New("nil session")
	}
	if a.Model == nil || a.Store == nil {
		return nil, errors.New("agent not fully wired")
	}

	// The sandbox comes with the session and the session owns it: a turn ending
	// is not a reason to release the handle, and the next turn of the same task
	// runs in the same filesystem. Binding one is the application's job, done
	// before the turn rather than inside it.
	box := sess.Sandbox()
	if box == nil {
		return nil, errors.New("session has no sandbox")
	}

	out := make(chan Event, 8)
	go func() {
		defer close(out)

		for {
			if err := ctx.Err(); err != nil {
				a.fail(ctx, sess, out, err)
				return
			}

			last := lastMessage(sess)
			calls := toolCalls(last)

			switch {
			case calls != nil: // assistant asked for tools
				for _, c := range calls {
					res := a.dispatch(ctx, sess, box, c)
					sess.Messages = append(sess.Messages, res)
					out <- Event{Type: EventToolResult, Message: &res}
				}
				if err := a.Store.Save(ctx, sess); err != nil {
					a.fail(ctx, sess, out, err)
					return
				}

			case last != nil && last.Role == fantasy.MessageRoleAssistant: // done
				sess.Status = StatusCompleted
				_ = a.Store.Save(ctx, sess)
				out <- Event{Type: EventDone, Message: last}
				return

			// OBS: this does not stream, there is also Stream method on fantasy.LanguageModel
			default: // empty, or last was user/tool -> call the model
				msg, usage, err := a.Model.Generate(ctx, model.ModelRequest{
					Model:    sess.Model,
					System:   a.SystemPrompt,
					Messages: sess.Messages,
					Tools:    a.toolDefs(),
				})
				if err != nil {
					a.fail(ctx, sess, out, err)
					return
				}
				sess.Messages = append(sess.Messages, msg)
				addUsage(&sess.Usage, usage)
				out <- Event{Type: EventAssistant, Message: &sess.Messages[len(sess.Messages)-1]}
				if err := a.Store.Save(ctx, sess); err != nil {
					a.fail(ctx, sess, out, err)
					return
				}
			}
		}
	}()

	return out, nil
}

// dispatch runs one tool against the sandbox and wraps the result as a tool message.
// A tool returning an error becomes an error result fed back to the model, NOT a fatal stop.
func (a *Agent) dispatch(ctx context.Context, sess *Session, box Sandbox, call fantasy.ToolCallPart) fantasy.Message {
	t, ok := a.Tools[call.ToolName]
	if !ok {
		return toolResultMsg(call.ToolCallID, "unknown tool: "+call.ToolName, true)
	}
	res, err := t.Execute(ctx, &ExecCtx{Session: sess, Sandbox: box}, json.RawMessage(call.Input))
	if err != nil {
		return toolResultMsg(call.ToolCallID, err.Error(), true)
	}
	return toolResultMsg(call.ToolCallID, res.Content, res.IsError)
}

func (a *Agent) fail(ctx context.Context, sess *Session, out chan<- Event, err error) {
	sess.Status = StatusFailed
	_ = a.Store.Save(ctx, sess)
	out <- Event{Type: EventError, Err: err}
}

func lastMessage(sess *Session) *fantasy.Message {
	if len(sess.Messages) == 0 {
		return nil
	}
	return &sess.Messages[len(sess.Messages)-1]
}

// toolCalls returns the tool-call parts of an assistant message, or nil.
func toolCalls(m *fantasy.Message) []fantasy.ToolCallPart {
	if m == nil || m.Role != fantasy.MessageRoleAssistant {
		return nil
	}
	var calls []fantasy.ToolCallPart
	for _, p := range m.Content {
		if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

func toolResultMsg(id, content string, isErr bool) fantasy.Message {
	var output fantasy.ToolResultOutputContent
	if isErr {
		output = fantasy.ToolResultOutputContentError{Error: errors.New(content)}
	} else {
		output = fantasy.ToolResultOutputContentText{Text: content}
	}
	return fantasy.Message{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: id, Output: output},
		},
	}
}
