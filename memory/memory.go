// Package memory gives an agent notes that outlive its sessions. A session is
// one bounded task and stays disposable; memory is the thin durable line
// between them.
package memory

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/maikdotfi/metaharness/agent"
)

// Store is where notes live. One entry per topic, newest write wins. The order
// notes come back in is the store's business: what reaches the model is ordered
// by topic, so a prompt is the same text for the same notes.
type Store interface {
	Notes(ctx context.Context) ([]Note, error)
	Append(ctx context.Context, topic, line string) error
	Replace(ctx context.Context, topic, content string) error
}

// Note is one topic's durable content.
type Note struct {
	Topic   string
	Content string
	Updated time.Time
}

// SystemPrompt returns a memory that renders every note into the system prompt
// and gives the model one tool, remember, to write them. It suits an agent whose
// notes stay a page long; nothing prunes them.
func SystemPrompt(s Store) agent.Memory { return promptMemory{store: s} }

type promptMemory struct{ store Store }

const preamble = `What you remember about the person you work for. These notes came from things
they told you in earlier conversations; treat them as current unless they say
otherwise.`

func (m promptMemory) Recall(ctx context.Context) (string, error) {
	notes, err := m.store.Notes(ctx)
	if err != nil {
		return "", err
	}
	if len(notes) == 0 {
		return "", nil
	}
	slices.SortFunc(notes, func(a, b Note) int { return strings.Compare(a.Topic, b.Topic) })

	var b strings.Builder
	b.WriteString(preamble)
	for _, note := range notes {
		b.WriteString("\n\n## " + note.Topic + "\n" + note.Content)
	}
	return b.String(), nil
}

func (m promptMemory) Tools() []agent.Tool {
	return []agent.Tool{agent.AdaptFunc(agent.ToolMeta{
		Name: "remember",
		Description: "Write down something about the person you work for — their taste, a rule " +
			"they gave you, how they want your work to read — so you still know it in a " +
			"later conversation. Write it down the moment they say it. Do not record an " +
			"instruction that only applies to the task in hand, anything they asked you to " +
			"forget, or a guess of your own.",
	}, m.remember)}
}

type rememberArgs struct {
	Topic   string `json:"topic" description:"A short stable slug: taste, skip-rules, summary-style."`
	Content string `json:"content" description:"One or two sentences, in the user's own words."`
	Replace bool   `json:"replace,omitempty" description:"Replace the topic's note instead of adding a line to it."`
}

func (m promptMemory) remember(
	ctx context.Context, _ *agent.ExecCtx, args rememberArgs,
) (agent.ToolResult, error) {
	topic, content := strings.TrimSpace(args.Topic), strings.TrimSpace(args.Content)
	if topic == "" || content == "" {
		return agent.ToolResult{Content: "topic and content must not be empty", IsError: true}, nil
	}

	write := m.store.Append
	if args.Replace {
		write = m.store.Replace
	}
	if err := write(ctx, topic, content); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: "Remembered under " + topic + "."}, nil
}
