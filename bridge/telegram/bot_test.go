package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/testutils"
	"github.com/maikdotfi/metaharness/tools"
)

// fakeAPI is the test double for telegramAPI. It records every call and hands
// out incrementing message ids so the status-message edit path is exercised.
type fakeAPI struct {
	mu      sync.Mutex
	sends   []bot.SendMessageParams
	edits   []bot.EditMessageTextParams
	actions int
	nextID  int
	sendErr error
}

func (f *fakeAPI) SendMessage(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, *p)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.nextID++
	return &models.Message{ID: f.nextID}, nil
}

func (f *fakeAPI) EditMessageText(_ context.Context, p *bot.EditMessageTextParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, *p)
	return &models.Message{ID: p.MessageID}, nil
}

func (f *fakeAPI) SendChatAction(_ context.Context, _ *bot.SendChatActionParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions++
	return true, nil
}

// sentTexts returns the text of every SendMessage call, in order.
func (f *fakeAPI) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sends))
	for i, s := range f.sends {
		out[i] = s.Text
	}
	return out
}

func (f *fakeAPI) editTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.edits))
	for i, e := range f.edits {
		out[i] = e.Text
	}
	return out
}

func (f *fakeAPI) actionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.actions
}

// --- helpers ---------------------------------------------------------------

func asstText(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

func asstToolCall(name, input string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: name, Input: input}},
	}
}

func privateText(userID, chatID int64, text string) *models.Update {
	return &models.Update{Message: &models.Message{
		Text: text,
		From: &models.User{ID: userID},
		Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate},
	}}
}

const allowedUser = int64(42)

// newTestBot wires a personalBot around a caller-supplied model, with an
// in-memory factory that hands out incrementing session ids. editGap is 0 so
// every progress step flushes, making edits deterministic.
func newTestBot(t *testing.T, m model.ModelClient, showThinking bool) (*personalBot, *fakeAPI) {
	t.Helper()
	a := agent.New("test system prompt",
		agent.WithModel(m),
		agent.WithTools(agent.Adapt(tools.Bash{})),
	)
	var n int
	factory := func() (*agent.Session, error) {
		n++
		box := &testutils.FakeSandbox{SandboxName: "work"}
		return agent.NewSession(fmt.Sprintf("sess_%d", n), "test-model", box), nil
	}
	first, err := factory()
	if err != nil {
		t.Fatalf("session factory: %v", err)
	}
	api := &fakeAPI{}
	pb := &personalBot{
		agent:        a,
		api:          api,
		newSession:   factory,
		allowed:      map[int64]bool{allowedUser: true},
		showThinking: showThinking,
		editGap:      0,
		now:          time.Now,
		current:      first,
	}
	return pb, api
}

func countEqual(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

// --- tests -----------------------------------------------------------------

func TestAllowedTextReachesAgentAndReplies(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("the answer")}}
	pb, api := newTestBot(t, m, false)

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "review this"))

	// The prompt reached the agent and the transcript recorded both turns.
	if len(m.Calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(m.Calls))
	}
	if got := len(pb.current.Messages); got != 2 {
		t.Fatalf("transcript length = %d, want 2 (user + assistant)", got)
	}
	if pb.current.Messages[0].Role != fantasy.MessageRoleUser {
		t.Fatalf("first message role = %v, want user", pb.current.Messages[0].Role)
	}

	// The answer was delivered exactly once, and never inside a progress edit.
	if n := countEqual(api.sentTexts(), "the answer"); n != 1 {
		t.Fatalf("answer sent %d times, want 1 (sends: %#v)", n, api.sentTexts())
	}
	if n := countEqual(api.editTexts(), "the answer"); n != 0 {
		t.Fatalf("answer leaked into %d progress edits", n)
	}
}

func TestUnauthorizedNeverInvokesAgent(t *testing.T) {
	m := &testutils.ScriptedModel{} // no replies: any model call would error
	pb, api := newTestBot(t, m, false)

	pb.handleUpdate(context.Background(), privateText(9999, 100, "let me in"))

	if len(m.Calls) != 0 {
		t.Fatalf("model was called %d times for an unauthorized user", len(m.Calls))
	}
	if len(pb.current.Messages) != 0 {
		t.Fatalf("unauthorized message mutated the transcript: %#v", pb.current.Messages)
	}
	if len(api.sentTexts()) != 0 {
		t.Fatalf("bridge replied to an unauthorized user: %#v", api.sentTexts())
	}
}

func TestGroupAndUnsupportedMessagesIgnored(t *testing.T) {
	m := &testutils.ScriptedModel{}
	pb, _ := newTestBot(t, m, false)
	ctx := context.Background()

	group := &models.Update{Message: &models.Message{
		Text: "hello", From: &models.User{ID: allowedUser},
		Chat: models.Chat{ID: 1, Type: models.ChatTypeGroup},
	}}
	nonText := privateText(allowedUser, 100, "") // e.g. a photo, no text
	nonText.Message.Photo = []models.PhotoSize{{FileID: "x"}}
	empty := &models.Update{} // no message at all

	for _, u := range []*models.Update{group, nonText, empty} {
		pb.handleUpdate(ctx, u)
	}

	if len(m.Calls) != 0 {
		t.Fatalf("model was called %d times for ignored messages", len(m.Calls))
	}
}

func TestConsecutiveMessagesShareSession(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a"), asstText("b")}}
	pb, _ := newTestBot(t, m, false)
	ctx := context.Background()

	id := pb.current.ID
	pb.handleUpdate(ctx, privateText(allowedUser, 100, "first"))
	pb.handleUpdate(ctx, privateText(allowedUser, 100, "second"))

	if pb.current.ID != id {
		t.Fatalf("session id changed across turns: %s -> %s", id, pb.current.ID)
	}
	if got := len(pb.current.Messages); got != 4 {
		t.Fatalf("transcript length = %d, want 4 (two full turns)", got)
	}
}

func TestNewAndClearReplaceSession(t *testing.T) {
	for _, cmd := range []string{"/new", "/clear"} {
		t.Run(cmd, func(t *testing.T) {
			m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a")}}
			pb, _ := newTestBot(t, m, false)
			ctx := context.Background()

			pb.handleUpdate(ctx, privateText(allowedUser, 100, "do work"))
			before := pb.current.ID
			if len(pb.current.Messages) == 0 {
				t.Fatal("expected a transcript before reset")
			}

			pb.handleUpdate(ctx, privateText(allowedUser, 100, cmd))

			if pb.current.ID == before {
				t.Fatalf("%s did not replace the session (still %s)", cmd, before)
			}
			if len(pb.current.Messages) != 0 {
				t.Fatalf("%s left %d messages; want a fresh transcript", cmd, len(pb.current.Messages))
			}
		})
	}
}

func TestCommandsNotAppendedToTranscript(t *testing.T) {
	m := &testutils.ScriptedModel{} // no replies: commands must not call the model
	pb, api := newTestBot(t, m, false)
	ctx := context.Background()

	for _, cmd := range []string{"/status", "/help", "/start", "/status@mybot"} {
		pb.handleUpdate(ctx, privateText(allowedUser, 100, cmd))
	}

	if len(m.Calls) != 0 {
		t.Fatalf("a command invoked the model %d times", len(m.Calls))
	}
	if len(pb.current.Messages) != 0 {
		t.Fatalf("commands leaked into the transcript: %#v", pb.current.Messages)
	}
	if len(api.sentTexts()) != 4 {
		t.Fatalf("expected one reply per command, got %d", len(api.sentTexts()))
	}
}

// blockingModel blocks in Generate until ctx is cancelled, then returns the
// cancellation error. It stands in for a long-running turn.
type blockingModel struct{}

func (blockingModel) Generate(ctx context.Context, _ model.ModelRequest) (fantasy.Message, fantasy.Usage, error) {
	<-ctx.Done()
	return fantasy.Message{}, fantasy.Usage{}, ctx.Err()
}

func TestTurnsSerialized(t *testing.T) {
	// If turns interleaved, both user messages would be appended before either
	// assistant reply, giving a user,user,... transcript. Serialization forces
	// strictly alternating user,assistant,user,assistant.
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a"), asstText("b")}}
	pb, _ := newTestBot(t, m, false)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			pb.handleUpdate(ctx, privateText(allowedUser, 100, "concurrent"))
		})
	}
	wg.Wait()

	if got := len(pb.current.Messages); got != 4 {
		t.Fatalf("transcript length = %d, want 4", got)
	}
	for i, msg := range pb.current.Messages {
		wantUser := i%2 == 0
		isUser := msg.Role == fantasy.MessageRoleUser
		if wantUser != isUser {
			t.Fatalf("message %d role = %v; transcript is interleaved: %+v", i, msg.Role, roles(pb.current.Messages))
		}
	}
}

func roles(ms []fantasy.Message) []fantasy.MessageRole {
	out := make([]fantasy.MessageRole, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func TestLongResponseSplitOnUTF8Boundaries(t *testing.T) {
	// No spaces: splitMessage hard-cuts at the rune limit, so a byte-based split
	// would corrupt these 3- and 4-byte runes. Reconstruction must be exact.
	long := strings.Repeat("世界🙂", 3000) // well over the 4096-rune limit
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText(long)}}
	pb, api := newTestBot(t, m, false)

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "say a lot"))

	sends := api.sentTexts()
	// sends[0] is the "…" progress placeholder; the rest are answer chunks.
	if len(sends) < 3 {
		t.Fatalf("expected the answer split across multiple sends, got %d sends", len(sends))
	}
	var rebuilt strings.Builder
	for _, chunk := range sends[1:] {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk is not valid UTF-8: %q", chunk)
		}
		if utf8.RuneCountInString(chunk) > telegramMaxMessage {
			t.Fatalf("chunk exceeds Telegram limit: %d runes", utf8.RuneCountInString(chunk))
		}
		rebuilt.WriteString(chunk)
	}
	if rebuilt.String() != long {
		t.Fatal("rejoined answer chunks do not reconstruct the original text")
	}
}

func TestProgressReportedAndFinalDeliveredOnce(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{
		asstToolCall("bash", `{"cmd":"ls -la"}`),
		asstText("all reviewed"),
	}}
	pb, api := newTestBot(t, m, false)

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "review"))

	edits := api.editTexts()
	if len(edits) == 0 {
		t.Fatal("no progress edits: progress was not reported during the turn")
	}
	// The last edit is the finished trail and should end with the done step and
	// include the tool step.
	trail := edits[len(edits)-1]
	if !strings.Contains(trail, "🔧 bash: ls -la") {
		t.Fatalf("progress trail missing the tool step: %q", trail)
	}
	if !strings.Contains(trail, "✅ done") {
		t.Fatalf("progress trail missing the done step: %q", trail)
	}
	// The typing action fired at least once.
	if api.actionCount() < 1 {
		t.Fatal("no typing chat action was sent")
	}
	// The answer was delivered exactly once and never as progress.
	if n := countEqual(api.sentTexts(), "all reviewed"); n != 1 {
		t.Fatalf("answer sent %d times, want 1", n)
	}
	if strings.Contains(trail, "all reviewed") {
		t.Fatalf("answer leaked into the progress trail: %q", trail)
	}
}

func TestAgentFailureSendsErrorWithoutReplay(t *testing.T) {
	m := &testutils.ScriptedModel{} // exhausted script -> Generate returns an error
	pb, api := newTestBot(t, m, false)

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "do it"))

	if len(m.Calls) != 1 {
		t.Fatalf("model called %d times; a failed turn must not be replayed", len(m.Calls))
	}
	sends := api.sentTexts()
	var failure string
	for _, s := range sends {
		if strings.HasPrefix(s, "Sorry, that turn failed") {
			failure = s
		}
	}
	if failure == "" {
		t.Fatalf("no failure message sent to the user (sends: %#v)", sends)
	}
	// The session remains usable for another message.
	if pb.current == nil {
		t.Fatal("session was discarded after a failure")
	}
}

func TestContextCancellationStopsActiveRun(t *testing.T) {
	pb, _ := newTestBot(t, blockingModel{}, false)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		pb.handleUpdate(ctx, privateText(allowedUser, 100, "long task"))
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleUpdate did not return after context cancellation")
	}
}
