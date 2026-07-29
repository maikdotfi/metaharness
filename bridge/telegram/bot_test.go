package telegram

import (
	"context"
	"errors"
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

// newTestBot wires a bridge the way an application does — one Config — around a
// caller-supplied model and a storage-free agent. editGap is 0 so every progress
// step flushes, making edits deterministic.
func newTestBot(t *testing.T, m model.ModelClient, showThinking bool) (*personalBot, *fakeAPI) {
	t.Helper()
	a := agent.New("test system prompt",
		agent.WithModel(m),
		agent.WithTools(agent.Adapt(tools.Bash{})),
	)
	return newTestBridge(t, Config{
		Token:        "test-token",
		Agent:        a,
		Sandboxes:    &testutils.FakeSandboxes{},
		SandboxName:  "work",
		Model:        "test-model",
		AllowedUsers: []int64{allowedUser},
		ShowThinking: showThinking,
	})
}

// newTestBridge builds the bridge from cfg and swaps in the test's Telegram
// double. Everything else — the first session, the id scheme, whether there is
// resumable history — is the bridge's own doing, which is the point.
func newTestBridge(t *testing.T, cfg Config) (*personalBot, *fakeAPI) {
	t.Helper()
	pb, err := newBridge(cfg)
	if err != nil {
		t.Fatalf("newBridge() error = %v", err)
	}
	api := &fakeAPI{}
	pb.api = api
	pb.editGap = 0
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

// TestBridgeStartsItsOwnSessions pins what an application no longer writes: it
// names the sandbox to work in and the model to work with, and the bridge starts
// the first task, gives every later one a fresh id, and keeps them all in that
// one filesystem.
func TestBridgeStartsItsOwnSessions(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a")}}
	pb, _ := newTestBot(t, m, false)
	ctx := context.Background()

	first := pb.current
	if first == nil {
		t.Fatal("the bridge has no session to talk to")
	}
	if first.ID == "" {
		t.Error("the first session has no id")
	}
	if got := first.SandboxName(); got != "work" {
		t.Errorf("first session works in %q, want the sandbox the bridge was given, %q", got, "work")
	}

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "/new"))

	if pb.current.ID == first.ID {
		t.Errorf("/new reused id %q; every task gets its own", first.ID)
	}
	if got := pb.current.SandboxName(); got != "work" {
		t.Errorf("the session after /new works in %q, want %q: /new keeps the files", got, "work")
	}
}

// TestBridgeOffersHistoryItDerives is the pair of the resumable wiring: nothing
// hands the bridge a session list, so /sessions can only work if the bridge asked
// the agent it was given for one.
func TestBridgeOffersHistoryItDerives(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a")}}
	pb, api := newResumableTestBot(t, m)
	ctx := context.Background()

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "do work"))
	worked := pb.current.ID

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "/sessions"))

	if got := lastSent(t, api); !strings.Contains(got, worked) {
		t.Errorf("/sessions = %q, want it to list session %q", got, worked)
	}
}

// TestBridgeWithoutSomewhereToWorkFailsToStart keeps a broken backend from
// producing a bridge with nowhere to run: it is reported to the application that
// can still act on it, not to a chat window later.
func TestBridgeWithoutSomewhereToWorkFailsToStart(t *testing.T) {
	a := agent.New("test system prompt", agent.WithModel(&testutils.ScriptedModel{}))
	_, err := newBridge(Config{
		Token:        "test-token",
		Agent:        a,
		Sandboxes:    &testutils.FakeSandboxes{OpenErr: errors.New("no backend")},
		SandboxName:  "work",
		Model:        "test-model",
		AllowedUsers: []int64{allowedUser},
	})
	if err == nil {
		t.Fatal("newBridge() succeeded with a sandbox opener that cannot open anything")
	}
}

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

// --- /sessions and /resume ---------------------------------------------------

// newResumableTestBot wires a bridge whose sessions are retained, so /sessions
// has something to list and /resume something to bring back — the wiring an
// application does when it gives its agent a store. It hands over no history of
// its own: an agent with a store and somewhere to open sandboxes is the whole of
// what resuming takes.
func newResumableTestBot(t *testing.T, m model.ModelClient) (*personalBot, *fakeAPI) {
	t.Helper()
	a := agent.New("test system prompt",
		agent.WithModel(m),
		agent.WithStore(&testutils.MemStore{}),
		agent.WithTools(agent.Adapt(tools.Bash{})),
	)
	return newTestBridge(t, Config{
		Token:        "test-token",
		Agent:        a,
		Sandboxes:    &testutils.FakeSandboxes{},
		SandboxName:  "work",
		Model:        "test-model",
		AllowedUsers: []int64{allowedUser},
	})
}

// lastSent is the text of the most recent reply, which for a command is the reply
// to that command.
func lastSent(t *testing.T, api *fakeAPI) string {
	t.Helper()
	texts := api.sentTexts()
	if len(texts) == 0 {
		t.Fatal("the bridge sent nothing")
	}
	return texts[len(texts)-1]
}

// TestResumeMakesAStoredSessionCurrent is the pair no store test can prove on its
// own: a session comes back from storage, gets bound to the sandbox it recorded,
// and the next message continues its transcript rather than starting a new one.
func TestResumeMakesAStoredSessionCurrent(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{
		asstText("first"), asstText("second"), asstText("third"),
	}}
	pb, _ := newResumableTestBot(t, m)
	ctx := context.Background()

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "remember this"))
	firstID := pb.current.ID
	stored := len(pb.current.Messages)
	if stored == 0 {
		t.Fatal("expected a transcript before starting a second session")
	}

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "/new"))
	if pb.current.ID == firstID {
		t.Fatal("/new did not replace the session")
	}

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "/resume "+firstID))

	if pb.current.ID != firstID {
		t.Fatalf("current session = %q, want the resumed %q", pb.current.ID, firstID)
	}
	if len(pb.current.Messages) != stored {
		t.Errorf("resumed session has %d messages, want the %d it was saved with",
			len(pb.current.Messages), stored)
	}
	if pb.current.Sandbox() == nil {
		t.Fatal("the resumed session has no live sandbox, so it cannot run")
	}

	// And it really is runnable: the next prompt continues that transcript.
	pb.handleUpdate(ctx, privateText(allowedUser, 100, "and now this"))
	if len(pb.current.Messages) <= stored {
		t.Errorf("a turn on the resumed session left %d messages, want more than %d",
			len(pb.current.Messages), stored)
	}
}

func TestResumeUnknownIDKeepsTheCurrentSession(t *testing.T) {
	m := &testutils.ScriptedModel{} // a failed resume must not call the model
	pb, api := newResumableTestBot(t, m)

	before := pb.current
	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "/resume nope"))

	if pb.current != before {
		t.Error("a failed resume replaced the working session")
	}
	if got := lastSent(t, api); !strings.Contains(got, "nope") {
		t.Errorf("reply = %q, want it to name the session it could not resume", got)
	}
}

func TestResumeWithoutAnIDExplainsItself(t *testing.T) {
	pb, api := newResumableTestBot(t, &testutils.ScriptedModel{})

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "/resume"))

	if got := lastSent(t, api); !strings.Contains(got, "/resume") {
		t.Errorf("reply = %q, want usage naming the command", got)
	}
}

func TestSessionsListsWhatCanBeResumed(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("a")}}
	pb, api := newResumableTestBot(t, m)
	ctx := context.Background()

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "do work"))
	current := pb.current.ID

	pb.handleUpdate(ctx, privateText(allowedUser, 100, "/sessions"))

	got := lastSent(t, api)
	if !strings.Contains(got, current) {
		t.Errorf("/sessions = %q, want it to list session %q", got, current)
	}
}

// TestSessionsWithoutRetentionSaysSo keeps the storage-free bridge honest: the
// commands exist on the type, but a bridge with nothing retained says it keeps no
// history rather than reporting an unknown command or an empty list.
func TestSessionsWithoutRetentionSaysSo(t *testing.T) {
	pb, api := newTestBot(t, &testutils.ScriptedModel{}, false)

	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "/sessions"))

	got := lastSent(t, api)
	if strings.Contains(got, "Unknown command") {
		t.Errorf("/sessions = %q, want an explanation rather than an unknown command", got)
	}
	if !strings.Contains(strings.ToLower(got), "history") {
		t.Errorf("/sessions = %q, want it to say no history is kept", got)
	}
}

// TestResumeWithoutRetentionSaysSo is the other half of the same nil: a bridge
// given no history explains that rather than reporting a missing session.
func TestResumeWithoutRetentionSaysSo(t *testing.T) {
	pb, api := newTestBot(t, &testutils.ScriptedModel{}, false)

	before := pb.current
	pb.handleUpdate(context.Background(), privateText(allowedUser, 100, "/resume sess_1"))

	if pb.current != before {
		t.Error("a resume the bridge cannot do replaced the working session")
	}
	got := lastSent(t, api)
	if strings.Contains(got, "Unknown command") {
		t.Errorf("/resume = %q, want an explanation rather than an unknown command", got)
	}
	if !strings.Contains(got, "cannot resume") {
		t.Errorf("/resume = %q, want it to say resuming is not available", got)
	}
}

// TestHelpOnlyOffersCommandsThatWork keeps the help text from advertising a
// capability the assembling application did not wire.
func TestHelpOnlyOffersCommandsThatWork(t *testing.T) {
	storageFree, freeAPI := newTestBot(t, &testutils.ScriptedModel{}, false)
	storageFree.handleUpdate(context.Background(), privateText(allowedUser, 100, "/help"))
	if got := lastSent(t, freeAPI); strings.Contains(got, "/resume") {
		t.Errorf("help on a storage-free bridge = %q, want no /resume", got)
	}

	resumable, resumableAPI := newResumableTestBot(t, &testutils.ScriptedModel{})
	resumable.handleUpdate(context.Background(), privateText(allowedUser, 100, "/help"))
	if got := lastSent(t, resumableAPI); !strings.Contains(got, "/resume") {
		t.Errorf("help on a resumable bridge = %q, want /resume offered", got)
	}
}
