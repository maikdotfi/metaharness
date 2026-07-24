package model

import "charm.land/fantasy"

// Message is a conversation turn understood by the agent and model.
type Message = fantasy.Message

// NewUserMessage creates a user turn for an agent session.
func NewUserMessage(text string) Message {
	return fantasy.NewUserMessage(text)
}

// NewToolResultMessage creates a tool turn containing results for model-issued
// tool calls.
func NewToolResultMessage(results ...ToolResult) Message {
	content := make([]fantasy.MessagePart, 0, len(results))
	for _, result := range results {
		var output fantasy.ToolResultOutputContent
		if result.Error != nil {
			output = fantasy.ToolResultOutputContentError{Error: result.Error}
		} else {
			output = fantasy.ToolResultOutputContentText{Text: result.Text}
		}
		content = append(content, fantasy.ToolResultPart{
			ToolCallID: result.ID,
			Output:     output,
		})
	}
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: content}
}

// TextPart is text emitted in a model message.
type TextPart struct {
	Text string
}

// ReasoningPart is thinking (reasoning) text emitted in a model message.
type ReasoningPart struct {
	Text string
}

// ToolCall is a tool invocation emitted by a model.
type ToolCall struct {
	ID    string
	Name  string
	Input string
}

// ToolResult is the result of a tool invocation.
type ToolResult struct {
	ID    string
	Text  string
	Error error
}

// TextParts returns the text portions of m in their original order.
func TextParts(m *Message) []TextPart {
	if m == nil {
		return nil
	}
	var parts []TextPart
	for _, part := range m.Content {
		if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			parts = append(parts, TextPart{Text: text.Text})
		}
	}
	return parts
}

// ReasoningParts returns the thinking (reasoning) portions of m in their
// original order. It is empty unless the model was configured with Thinking.
func ReasoningParts(m *Message) []ReasoningPart {
	if m == nil {
		return nil
	}
	var parts []ReasoningPart
	for _, part := range m.Content {
		if reasoning, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
			parts = append(parts, ReasoningPart{Text: reasoning.Text})
		}
	}
	return parts
}

// ToolCalls returns the tool invocations in m in their original order.
func ToolCalls(m *Message) []ToolCall {
	if m == nil {
		return nil
	}
	var calls []ToolCall
	for _, part := range m.Content {
		if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
			calls = append(calls, ToolCall{
				ID:    call.ToolCallID,
				Name:  call.ToolName,
				Input: call.Input,
			})
		}
	}
	return calls
}

// ToolResults returns successful and failed tool outputs in m.
func ToolResults(m *Message) []ToolResult {
	if m == nil {
		return nil
	}
	var results []ToolResult
	for _, part := range m.Content {
		result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			continue
		}
		item := ToolResult{ID: result.ToolCallID}
		switch output := result.Output.(type) {
		case fantasy.ToolResultOutputContentText:
			item.Text = output.Text
		case fantasy.ToolResultOutputContentError:
			item.Error = output.Error
		}
		results = append(results, item)
	}
	return results
}
