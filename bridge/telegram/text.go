package telegram

import (
	"encoding/json"
	"strings"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

// telegramMaxMessage is Telegram's per-message text limit. Longer replies are
// split into several messages.
const telegramMaxMessage = 4096

// finalText extracts the deliverable answer from a terminal assistant message:
// its text parts joined in order, with surrounding whitespace trimmed. Tool
// calls and reasoning are deliberately excluded — they feed progress, never the
// answer.
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

// splitMessage breaks s into chunks no longer than limit runes, preferring to
// break at a newline and then a space in the back half of each window so it
// avoids slicing mid-word when it can. Because it operates on runes, every
// chunk is valid UTF-8. An empty string yields no chunks.
func splitMessage(s string, limit int) []string {
	if limit <= 0 {
		limit = telegramMaxMessage
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	if len(runes) <= limit {
		return []string{s}
	}

	var chunks []string
	for len(runes) > limit {
		cut, drop := breakPoint(runes[:limit])
		chunks = append(chunks, string(runes[:cut]))
		next := cut
		if drop {
			next++ // skip the single separator we broke on
		}
		runes = runes[next:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

// breakPoint chooses how many leading runes of a full-length window to emit as
// one chunk. It prefers the last newline, then the last space, but only in the
// back half so chunks stay reasonably full; otherwise it hard-cuts at the end.
// drop reports whether the rune at the returned index is a separator to skip
// rather than emit.
func breakPoint(window []rune) (cut int, drop bool) {
	limit := len(window)
	if i := lastIndexRune(window, '\n'); i >= limit/2 {
		return i, true
	}
	if i := lastIndexRune(window, ' '); i >= limit/2 {
		return i, true
	}
	return limit, false
}

func lastIndexRune(runes []rune, target rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

// stepLine renders one agent event as a short progress line, plus whether it
// should be shown. It has no Telegram dependency, so a future transport can
// reuse it directly. It does NOT imply a generic bridge or transport interface.
//
// A text-only assistant message returns ok=false: that text is the answer,
// delivered separately on EventDone, and must not appear as a progress step.
func stepLine(ev agent.Event, showThinking bool) (line string, ok bool) {
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

	// Text-only assistant message: this is the answer, delivered on EventDone.
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

// summarizeToolInput turns a tool call's JSON input into a compact one-line
// summary, favoring the most meaningful field when the input is a JSON object.
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
