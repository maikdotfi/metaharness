// Package telegram is a personal Telegram bridge for a single assembled Meta
// Harness agent. It talks to Telegram through long polling only, exposes no HTTP
// listener, and keeps one current agent session in memory.
//
// It is deliberately a Telegram integration, not a generic bridge or transport
// framework: the assembling application owns model, tools, sandbox, and prompt,
// and hands this package a wired *agent.Agent plus a factory for fresh sessions.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

// defaultEditGap throttles progress-status edits so a fast tool loop does not
// hit Telegram's editMessageText rate limit. The final trail is always flushed
// regardless of this gap.
const defaultEditGap = 700 * time.Millisecond

// typingInterval refreshes the "typing…" chat action while a turn runs. Telegram
// clears the action after ~5s, so we refresh a little sooner.
const typingInterval = 4 * time.Second

// SessionFactory returns a fresh session with a new opaque ID, the chosen model,
// an active status, and any desired sandbox specification. It keeps model and
// sandbox selection in the assembling application.
type SessionFactory func() *agent.Session

// SandboxSource reports live sandbox state so /status can say whether the
// workbench is awake. *agent.Registry satisfies it. It is an optional seam, not a
// requirement: a bridge without one reports the sandbox's name only.
type SandboxSource interface {
	Snapshot() []agent.SandboxInfo
}

// Config configures the personal bridge.
type Config struct {
	Token        string
	Agent        *agent.Agent
	NewSession   SessionFactory
	AllowedUsers []int64

	// Sandboxes optionally supplies live sandbox state for /status.
	Sandboxes SandboxSource

	// ShowThinking includes the model's raw reasoning text in the progress
	// status message. Progress itself is always reported and is not optional;
	// this flag only chooses between exposing the reasoning text and showing a
	// bare "thinking…" step. There is deliberately no flag to disable progress.
	ShowThinking bool
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("telegram: empty token")
	}
	if c.Agent == nil {
		return errors.New("telegram: nil Agent")
	}
	if c.NewSession == nil {
		return errors.New("telegram: nil NewSession")
	}
	if len(c.AllowedUsers) == 0 {
		return errors.New("telegram: at least one allowed user required")
	}
	return nil
}

// telegramAPI is the small slice of the Telegram client the bridge actually
// uses. It is an implementation seam for testing, not a public bridge
// abstraction: *bot.Bot satisfies it, and tests supply a fake.
type telegramAPI interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	EditMessageText(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error)
	SendChatAction(ctx context.Context, params *bot.SendChatActionParams) (bool, error)
}

// personalBot owns the single current session and serializes turns. There is
// deliberately no Telegram chat-to-session mapping: the allowlisted personal
// user talks to whichever session is current.
type personalBot struct {
	agent        *agent.Agent
	api          telegramAPI
	newSession   SessionFactory
	sandboxes    SandboxSource
	allowed      map[int64]bool
	showThinking bool

	editGap time.Duration
	now     func() time.Time

	mu      sync.Mutex // serializes turns and guards current
	current *agent.Session
}

// Run assembles the bridge and blocks, polling Telegram until ctx is cancelled.
// It never starts an HTTP listener. Invalid configuration fails before polling
// begins.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	allowed := make(map[int64]bool, len(cfg.AllowedUsers))
	for _, id := range cfg.AllowedUsers {
		allowed[id] = true
	}

	pb := &personalBot{
		agent:        cfg.Agent,
		newSession:   cfg.NewSession,
		sandboxes:    cfg.Sandboxes,
		allowed:      allowed,
		showThinking: cfg.ShowThinking,
		editGap:      defaultEditGap,
		now:          time.Now,
		current:      cfg.NewSession(),
	}

	opts := []bot.Option{
		// Only message updates; unsupported update types never reach us.
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message"}),
		// One worker processing updates synchronously keeps turns in order.
		bot.WithNotAsyncHandlers(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			pb.handleUpdate(ctx, update)
		}),
		bot.WithErrorsHandler(func(err error) {
			slog.Error("telegram polling error", "err", err)
		}),
	}

	b, err := bot.New(cfg.Token, opts...)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	pb.api = b

	// Long polling and webhooks are mutually exclusive. Remove any webhook and
	// drop queued updates so downtime does not replay stale prompts on startup.
	if _, err := b.DeleteWebhook(ctx, &bot.DeleteWebhookParams{DropPendingUpdates: true}); err != nil {
		slog.Warn("telegram delete webhook failed", "err", err)
	}

	slog.Info("telegram bridge started", "session", pb.current.ID, "allowed_users", len(allowed))
	b.Start(ctx) // blocks until ctx is cancelled
	return ctx.Err()
}

// handleUpdate applies the authorization and private-text checks, then routes to
// a command or an agent turn. Anything that fails a check is silently ignored so
// the bridge never reveals its configuration to strangers.
func (b *personalBot) handleUpdate(ctx context.Context, update *models.Update) {
	msg := update.Message
	if msg == nil {
		return // not a message update
	}
	if msg.Chat.Type != models.ChatTypePrivate {
		return // private chats only; no groups, channels, or forums
	}
	if msg.Text == "" {
		return // text only; ignore media and service messages
	}
	if msg.From == nil || !b.allowed[msg.From.ID] {
		slog.Warn("telegram unauthorized message ignored", "chat", msg.Chat.ID)
		return // never reaches the agent
	}

	chatID := msg.Chat.ID
	if strings.HasPrefix(msg.Text, "/") {
		b.handleCommand(ctx, chatID, commandName(msg.Text))
		return
	}
	b.handlePrompt(ctx, chatID, msg.Text)
}

// commandName extracts the bare command from message text, dropping any
// "@botname" suffix and arguments: "/status@mybot extra" -> "/status".
func commandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	cmd := fields[0]
	if at := strings.IndexByte(cmd, '@'); at >= 0 {
		cmd = cmd[:at]
	}
	return cmd
}

const helpText = `I run a Meta Harness agent. Just send a message to give it a task.

Commands:
/new, /clear — discard the current context and start a fresh task, keeping the sandbox
/status — show the current session id, model, sandbox, message count, and token usage
/help, /start — show this help`

// handleCommand runs a bridge command. Commands take the same turn lock, so one
// received during a turn takes effect only after that turn completes, and they
// are never appended to the agent transcript.
func (b *personalBot) handleCommand(ctx context.Context, chatID int64, cmd string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch cmd {
	case "/new", "/clear":
		previous := b.current
		b.current = b.newSession()
		// The sandbox is the workbench and the session is the task, so a fresh
		// task stays at the same bench. A factory that names a sandbox itself
		// still wins.
		if b.current.Sandbox == (agent.SandboxSpec{}) {
			b.current.Sandbox = previous.Sandbox
		}
		b.send(ctx, chatID, "Started a fresh session: "+b.current.ID+
			"\nsandbox "+b.sandboxStatus(b.current)+" (kept)")
	case "/status":
		s := b.current
		b.send(ctx, chatID, fmt.Sprintf(
			"session %s\nmodel %s\nsandbox %s\nmessages %d\ntokens: %d in / %d out",
			s.ID, s.Model, b.sandboxStatus(s), len(s.Messages),
			s.Usage.InputTokens, s.Usage.OutputTokens,
		))
	case "/help", "/start":
		b.send(ctx, chatID, helpText)
	default:
		b.send(ctx, chatID, "Unknown command.\n\n"+helpText)
	}
}

// sandboxStatus describes the sandbox a session works in: its name, plus whether
// it is awake when the application wired a source of live state. A session with no
// named sandbox gets a throwaway one per turn, which is worth saying out loud.
func (b *personalBot) sandboxStatus(sess *agent.Session) string {
	spec := b.agent.SandboxFor(sess)
	if spec.Name == "" {
		return "ephemeral"
	}
	if b.sandboxes != nil {
		for _, info := range b.sandboxes.Snapshot() {
			if info.Name == spec.Name {
				return fmt.Sprintf("%s (%s)", info.Name, info.State)
			}
		}
	}
	return spec.Name
}

// handlePrompt runs one agent turn for a text message: it appends the prompt to
// the current session, runs the agent, reports progress as events arrive, and
// delivers the final assistant text once, on EventDone.
func (b *personalBot) handlePrompt(ctx context.Context, chatID int64, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sess := b.current
	sess.Messages = append(sess.Messages, model.NewUserMessage(text))
	sess.Status = agent.StatusActive

	stopTyping := b.keepTyping(ctx, chatID)
	defer stopTyping()

	status := b.newStatus(chatID)
	status.start(ctx)

	events, err := b.agent.Run(ctx, sess)
	if err != nil {
		status.append(ctx, "❌ "+clip(oneLine(err.Error()), 120))
		status.flush(ctx, true)
		b.send(ctx, chatID, "Sorry, I couldn't start that turn: "+err.Error())
		return
	}

	var (
		final  string
		runErr error
	)
	for ev := range events {
		if line, ok := stepLine(ev, b.showThinking); ok {
			status.append(ctx, line)
		}
		switch ev.Type {
		case agent.EventDone:
			// The final assistant text arrives twice — once as EventAssistant
			// and again inside EventDone. Delivering solely on EventDone avoids
			// double-sending.
			final = finalText(ev.Message)
		case agent.EventError:
			runErr = ev.Err
		}
	}
	status.flush(ctx, true)

	if runErr != nil {
		// The session stays available for another message or /new. We do not
		// rerun the turn, which could repeat tool side effects.
		b.send(ctx, chatID, "Sorry, that turn failed: "+runErr.Error())
		return
	}
	if final == "" {
		b.send(ctx, chatID, "(the agent finished without any text to show)")
		return
	}
	for _, chunk := range splitMessage(final, telegramMaxMessage) {
		b.send(ctx, chatID, chunk)
	}
}

// keepTyping sends a typing chat action immediately and refreshes it on a ticker
// until the returned stop is called. Liveness is ephemeral: it adds nothing to
// the transcript.
func (b *personalBot) keepTyping(ctx context.Context, chatID int64) (stop func()) {
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		for {
			_, _ = b.api.SendChatAction(tctx, &bot.SendChatActionParams{
				ChatID: chatID,
				Action: models.ChatActionTyping,
			})
			select {
			case <-tctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return cancel
}

func (b *personalBot) send(ctx context.Context, chatID int64, text string) {
	if _, err := b.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Error("telegram send failed", "err", err)
	}
}

// statusMessage is the single progress message for one turn. It is created as a
// placeholder and then edited in place as steps arrive, rather than posting a
// new message per step.
type statusMessage struct {
	api    telegramAPI
	chatID int64
	minGap time.Duration
	now    func() time.Time

	id     int // Telegram message id; 0 until the placeholder is created
	lines  []string
	shown  string
	lastAt time.Time
}

func (b *personalBot) newStatus(chatID int64) *statusMessage {
	return &statusMessage{api: b.api, chatID: chatID, minGap: b.editGap, now: b.now}
}

func (s *statusMessage) start(ctx context.Context) {
	msg, err := s.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: s.chatID, Text: "…"})
	if err != nil {
		slog.Error("telegram status create failed", "err", err)
	} else if msg != nil {
		s.id = msg.ID
	}
	s.lastAt = s.now()
}

func (s *statusMessage) append(ctx context.Context, line string) {
	s.lines = append(s.lines, line)
	s.flush(ctx, false)
}

// flush edits the status message to the current trail. Unless force is set it
// respects minGap to avoid Telegram's edit rate limit, and it never re-sends
// unchanged text.
func (s *statusMessage) flush(ctx context.Context, force bool) {
	if len(s.lines) == 0 {
		return
	}
	if !force && s.now().Sub(s.lastAt) < s.minGap {
		return
	}
	text := strings.Join(s.lines, "\n")
	if text == s.shown {
		return
	}

	if s.id == 0 {
		if msg, err := s.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: s.chatID, Text: text}); err != nil {
			slog.Error("telegram status send failed", "err", err)
		} else if msg != nil {
			s.id = msg.ID
		}
	} else if _, err := s.api.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: s.chatID, MessageID: s.id, Text: text,
	}); err != nil {
		slog.Error("telegram status edit failed", "err", err)
	}
	s.shown = text
	s.lastAt = s.now()
}
