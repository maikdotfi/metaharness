package xmpp

import (
	"encoding/json"
	"strings"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

func finalText(m *fantasy.Message) string {
	var b strings.Builder
	for _, part := range model.TextParts(m) {
		if part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return strings.TrimSpace(b.String())
}

func stepLine(ev agent.Event, showThinking bool) (string, bool) {
	switch ev.Type {
	case agent.EventAssistant:
		return assistantLine(ev.Message, showThinking)
	case agent.EventToolResult:
		return toolResultLine(ev.Message)
	case agent.EventDone:
		return "✅ done", true
	case agent.EventError:
		if ev.Err != nil {
			return "❌ " + clip(oneLine(ev.Err.Error()), 120), true
		}
		return "❌ failed", true
	default:
		return "", false
	}
}

func assistantLine(m *fantasy.Message, showThinking bool) (string, bool) {
	if calls := model.ToolCalls(m); len(calls) > 0 {
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			if arg := summarizeToolInput(c.Input); arg != "" {
				parts = append(parts, "🔧 "+c.Name+": "+arg)
			} else {
				parts = append(parts, "🔧 "+c.Name)
			}
		}
		return strings.Join(parts, "\n"), true
	}

	if reasoning := model.ReasoningParts(m); len(reasoning) > 0 {
		if !showThinking {
			return "💭 thinking…", true
		}
		var b strings.Builder
		for _, r := range reasoning {
			if r.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(oneLine(r.Text))
		}
		if b.Len() == 0 {
			return "💭 thinking…", true
		}
		return "💭 " + clip(b.String(), 240), true
	}
	return "", false
}

func toolResultLine(m *fantasy.Message) (string, bool) {
	results := model.ToolResults(m)
	if len(results) == 0 {
		return "", false
	}
	for _, r := range results {
		if r.Error != nil {
			return "⚠️ " + clip(oneLine(r.Error.Error()), 120), true
		}
	}
	last := results[len(results)-1]
	if summary := clip(oneLine(last.Text), 80); summary != "" {
		return "↳ " + summary, true
	}
	return "↳ ok", true
}

func summarizeToolInput(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(input), &obj); err == nil {
		for _, key := range []string{"cmd", "command", "path", "file_path", "file", "skill", "query"} {
			if s, ok := obj[key].(string); ok && s != "" {
				return clip(oneLine(s), 60)
			}
		}
	}
	return clip(oneLine(input), 60)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

func clip(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
