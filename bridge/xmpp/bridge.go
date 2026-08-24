// Package xmpp is a personal XMPP bridge for a single assembled Meta Harness
// agent. It connects as a normal XMPP client account, accepts direct chat
// messages from an allowlist, and keeps one current agent session in memory.
//
// The Username in Config is a full XMPP JID such as agent@example.org. The
// domain is used for normal XMPP service discovery, so installations such as
// Snikket need no transport-specific server setting when their DNS is set up.
package xmpp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	mxmpp "mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
)

const defaultCorrectionGap = 700 * time.Millisecond

// Config configures the personal XMPP bridge.
type Config struct {
	// Username is the full JID of the account the bridge logs in as, for example
	// agent@example.org. It may include a resource; without one the server assigns
	// a resource during binding.
	Username string
	Password string
	Agent    *agent.Agent

	// Sandboxes is where the bridge opens sandboxes, and SandboxName is the one
	// every session works in. /new discards conversation context but keeps files.
	Sandboxes   agent.SandboxOpener
	SandboxName string

	// Model is the model id every session starts on.
	Model string

	// AllowedJIDs contains the people permitted to use the agent. Each entry must
	// be a user JID. Authorization compares bare JIDs, so every resource belonging
	// to an allowed account is accepted.
	AllowedJIDs []string

	// ShowThinking includes raw model reasoning in progress messages. Progress is
	// always reported; false replaces reasoning text with a bare "thinking…" step.
	ShowThinking bool
}

func (c Config) validate() (jid.JID, map[string]bool, error) {
	username := strings.TrimSpace(c.Username)
	if username == "" {
		return jid.JID{}, nil, errors.New("xmpp: empty Username")
	}
	account, err := jid.Parse(username)
	if err != nil || account.Localpart() == "" || account.Domainpart() == "" {
		if err == nil {
			err = errors.New("a user and domain are required")
		}
		return jid.JID{}, nil, fmt.Errorf("xmpp: invalid Username %q: %w", username, err)
	}
	if c.Password == "" {
		return jid.JID{}, nil, errors.New("xmpp: empty Password")
	}
	if c.Agent == nil {
		return jid.JID{}, nil, errors.New("xmpp: nil Agent")
	}
	if c.Sandboxes == nil {
		return jid.JID{}, nil, errors.New("xmpp: nil Sandboxes: nowhere to open the sandbox to work in")
	}
	if strings.TrimSpace(c.SandboxName) == "" {
		return jid.JID{}, nil, errors.New("xmpp: empty SandboxName")
	}
	if strings.TrimSpace(c.Model) == "" {
		return jid.JID{}, nil, errors.New("xmpp: empty Model")
	}
	if len(c.AllowedJIDs) == 0 {
		return jid.JID{}, nil, errors.New("xmpp: at least one allowed JID required")
	}

	allowed := make(map[string]bool, len(c.AllowedJIDs))
	for _, raw := range c.AllowedJIDs {
		raw = strings.TrimSpace(raw)
		j, err := jid.Parse(raw)
		if err != nil || j.Localpart() == "" || j.Domainpart() == "" {
			if err == nil {
				err = errors.New("a user and domain are required")
			}
			return jid.JID{}, nil, fmt.Errorf("xmpp: invalid allowed JID %q: %w", raw, err)
		}
		allowed[j.Bare().String()] = true
	}
	return account, allowed, nil
}

// xmppAPI is the small outbound seam used by the bridge. A replaceID requests
// XEP-0308 last-message correction, which lets supporting clients display the
// progress trail as one edited message.
type xmppAPI interface {
	SendText(ctx context.Context, to jid.JID, text, replaceID string) (messageID string, err error)
}

type sessionAPI struct {
	session *mxmpp.Session
}

type replacement struct {
	ID string `xml:"id,attr"`
}

// messageBody is the subset of a direct chat message the bridge sends and
// receives. Replace implements XEP-0308 without coupling the bridge to a second
// extension package.
type messageBody struct {
	stanza.Message
	Body    string        `xml:"body"`
	Replace *replacement  `xml:"urn:xmpp:message-correct:0 replace,omitempty"`
	Delay   *stanza.Delay `xml:"urn:xmpp:delay delay,omitempty"`
}

func (a sessionAPI) SendText(ctx context.Context, to jid.JID, text, replaceID string) (string, error) {
	id := newMessageID()
	msg := messageBody{
		Message: stanza.Message{ID: id, To: to, Type: stanza.ChatMessage},
		Body:    text,
	}
	if replaceID != "" {
		msg.Replace = &replacement{ID: replaceID}
	}
	if err := a.session.Encode(ctx, msg); err != nil {
		return "", err
	}
	return id, nil
}

type personalBridge struct {
	account      jid.JID
	agent        *agent.Agent
	api          xmppAPI
	boxes        agent.SandboxOpener
	sandboxName  string
	modelID      string
	sessions     agent.Sessions
	allowed      map[string]bool
	showThinking bool

	correctionGap time.Duration
	now           func() time.Time

	mu      sync.Mutex
	current *agent.Session
}

func newBridge(cfg Config) (*personalBridge, error) {
	account, allowed, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	b := &personalBridge{
		account:       account,
		agent:         cfg.Agent,
		boxes:         cfg.Sandboxes,
		sandboxName:   cfg.SandboxName,
		modelID:       cfg.Model,
		sessions:      cfg.Agent.Sessions(cfg.Sandboxes),
		allowed:       allowed,
		showThinking:  cfg.ShowThinking,
		correctionGap: defaultCorrectionGap,
		now:           time.Now,
	}
	first, err := b.startSession()
	if err != nil {
		return nil, fmt.Errorf("xmpp: starting the first session: %w", err)
	}
	b.current = first
	return b, nil
}

func (b *personalBridge) startSession() (*agent.Session, error) {
	box, err := b.boxes.Open(b.sandboxName)
	if err != nil {
		return nil, err
	}
	return agent.NewSession(newSessionID(), b.modelID, box), nil
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("xmpp: reading random bytes: " + err.Error())
	}
	return "sess_" + hex.EncodeToString(b[:])
}

func newMessageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("xmpp: reading random bytes: " + err.Error())
	}
	return "msg_" + hex.EncodeToString(b[:])
}

// Run connects the account, announces availability, and blocks until ctx is
// cancelled or the XMPP stream ends. DialClientSession performs standard XMPP
// DNS discovery from Username's domain. TLS is mandatory before password auth.
func Run(ctx context.Context, cfg Config) error {
	b, err := newBridge(cfg)
	if err != nil {
		return err
	}
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closeCurrent()
	}()

	s, err := mxmpp.DialClientSession(ctx, b.account,
		mxmpp.BindResource(),
		mxmpp.StartTLS(nil),
		mxmpp.SASL("", cfg.Password,
			sasl.ScramSha256Plus,
			sasl.ScramSha256,
			sasl.ScramSha1Plus,
			sasl.ScramSha1,
			sasl.Plain,
		),
	)
	if err != nil {
		return fmt.Errorf("xmpp: connecting %s: %w", b.account.Bare(), err)
	}
	defer s.Conn().Close()
	defer s.Close()
	b.api = sessionAPI{session: s}

	if err := s.Send(ctx, stanza.Presence{Type: stanza.AvailablePresence}.Wrap(nil)); err != nil {
		return fmt.Errorf("xmpp: sending initial presence: %w", err)
	}

	incoming := newMessageQueue()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.Serve(incomingHandler(ctx, incoming))
	}()

	slog.Info("xmpp bridge started",
		"jid", s.LocalAddr().String(), "session", b.current.ID,
		"sandbox", b.sandboxName, "allowed_jids", len(b.allowed))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-serveDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return errors.New("xmpp: stream closed")
			}
			return fmt.Errorf("xmpp: serving stream: %w", err)
		case <-incoming.ready:
			if msg, ok := incoming.pop(); ok {
				b.handleMessage(ctx, msg)
			}
		}
	}
}

// messageQueue lets the stream handler finish before a turn sends anything on
// the same XMPP session. Mellium holds the stream locks while a handler runs, so
// blocking the handler on a full channel while the turn tries to send progress
// would otherwise deadlock. The outer bridge still consumes one item at a time.
type messageQueue struct {
	mu    sync.Mutex
	items []messageBody
	ready chan struct{}
}

func newMessageQueue() *messageQueue {
	return &messageQueue{ready: make(chan struct{}, 1)}
}

func (q *messageQueue) push(msg messageBody) {
	q.mu.Lock()
	q.items = append(q.items, msg)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *messageQueue) pop() (messageBody, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return messageBody{}, false
	}
	msg := q.items[0]
	q.items[0] = messageBody{}
	q.items = q.items[1:]
	if len(q.items) > 0 {
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
	return msg, true
}

func incomingHandler(ctx context.Context, incoming *messageQueue) mxmpp.Handler {
	return mxmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
		msg, ok, err := decodeMessage(t, start)
		if err != nil || !ok {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		incoming.push(msg)
		return nil
	})
}

func decodeMessage(r xml.TokenReader, start *xml.StartElement) (messageBody, bool, error) {
	if start.Name.Local != "message" ||
		(start.Name.Space != "" && start.Name.Space != stanza.NSClient) {
		return messageBody{}, false, nil
	}
	var msg messageBody
	err := xml.NewTokenDecoder(r).DecodeElement(&msg, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return messageBody{}, false, err
	}
	// A correction is the XMPP equivalent of an edited update. The original may
	// already be running, so treating the correction as another prompt could
	// repeat tool side effects. Delayed messages were queued while the bridge was
	// offline; ignoring them matches the Telegram bridge's stale-update policy.
	if msg.Type != stanza.ChatMessage || msg.Body == "" || msg.Replace != nil || msg.Delay != nil {
		return messageBody{}, false, nil
	}
	return msg, true, nil
}

func (b *personalBridge) handleMessage(ctx context.Context, msg messageBody) {
	from := msg.From.Bare()
	if from.Localpart() == "" || !b.allowed[from.String()] {
		slog.Warn("xmpp unauthorized message ignored", "from", from.String())
		return
	}
	if strings.HasPrefix(msg.Body, "/") {
		cmd, arg := splitCommand(msg.Body)
		b.handleCommand(ctx, from, cmd, arg)
		return
	}
	b.handlePrompt(ctx, from, msg.Body)
}

func splitCommand(text string) (cmd, arg string) {
	cmd, arg, _ = strings.Cut(strings.TrimSpace(text), " ")
	return cmd, strings.TrimSpace(arg)
}

const sessionListLimit = 10

func (b *personalBridge) helpText() string {
	var h strings.Builder
	h.WriteString("I run a Meta Harness agent. Just send a message to give it a task.\n\nCommands:\n")
	h.WriteString("/new, /clear — discard the current context and start a fresh task\n")
	h.WriteString("/status — show the current session id, model, message count, and token usage\n")
	if b.sessions != nil {
		h.WriteString("/sessions — list the sessions that can be resumed\n")
		h.WriteString("/resume <id> — continue a stored session where it left off\n")
	}
	h.WriteString("/help, /start — show this help")
	return h.String()
}

func (b *personalBridge) handleCommand(ctx context.Context, to jid.JID, cmd, arg string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch cmd {
	case "/new", "/clear":
		next, err := b.startSession()
		if err != nil {
			b.send(ctx, to, "Sorry, I couldn't start a fresh session: "+err.Error())
			return
		}
		b.closeCurrent()
		b.current = next
		b.send(ctx, to, "Started a fresh session: "+next.ID)
	case "/status":
		s := b.current
		b.send(ctx, to, fmt.Sprintf(
			"session %s\nsandbox %s\nmodel %s\nmessages %d\ntokens: %d in / %d out",
			s.ID, s.SandboxName(), s.Model, len(s.Messages), s.Usage.InputTokens, s.Usage.OutputTokens,
		))
	case "/sessions":
		b.listSessions(ctx, to)
	case "/resume":
		b.resumeSession(ctx, to, arg)
	case "/help", "/start":
		b.send(ctx, to, b.helpText())
	default:
		b.send(ctx, to, "Unknown command.\n\n"+b.helpText())
	}
}

func (b *personalBridge) listSessions(ctx context.Context, to jid.JID) {
	if b.sessions == nil {
		b.send(ctx, to, "This bridge keeps no session history.")
		return
	}
	infos, err := b.sessions.List(ctx, sessionListLimit)
	if err != nil {
		b.send(ctx, to, "Sorry, I couldn't list the sessions: "+err.Error())
		return
	}
	if len(infos) == 0 {
		b.send(ctx, to, "No stored sessions yet.")
		return
	}

	var out strings.Builder
	out.WriteString("Sessions, most recent first:\n")
	for _, info := range infos {
		fmt.Fprintf(&out, "\n%s — %d messages, %d tokens in / %d out",
			info.ID, info.Messages, info.Usage.InputTokens, info.Usage.OutputTokens)
		if info.ID == b.current.ID {
			out.WriteString(" (current)")
		}
	}
	out.WriteString("\n\nSend /resume <id> to continue one.")
	b.send(ctx, to, out.String())
}

func (b *personalBridge) resumeSession(ctx context.Context, to jid.JID, id string) {
	if b.sessions == nil {
		b.send(ctx, to, "This bridge cannot resume sessions.")
		return
	}
	if id == "" {
		b.send(ctx, to, "Usage: /resume <session id>")
		return
	}
	if id == b.current.ID {
		b.send(ctx, to, "That session is already the current one.")
		return
	}

	next, err := b.sessions.Resume(ctx, id)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			b.send(ctx, to, fmt.Sprintf("I have no session called %q.", id))
			return
		}
		b.send(ctx, to, fmt.Sprintf("Sorry, I couldn't resume %q: %s", id, err.Error()))
		return
	}
	b.closeCurrent()
	b.current = next
	b.send(ctx, to, fmt.Sprintf("Resumed %s: %d messages, working in %s.",
		next.ID, len(next.Messages), next.SandboxName()))
}

func (b *personalBridge) closeCurrent() {
	if b.current == nil {
		return
	}
	if err := b.current.Close(); err != nil {
		slog.Warn("releasing the session's sandbox failed", "session", b.current.ID, "err", err)
	}
}

func (b *personalBridge) handlePrompt(ctx context.Context, to jid.JID, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sess := b.current
	sess.Messages = append(sess.Messages, model.NewUserMessage(text))
	sess.Status = agent.StatusActive

	status := b.newStatus(to)
	status.start(ctx)

	events, err := b.agent.Run(ctx, sess)
	if err != nil {
		status.append(ctx, "❌ "+clip(oneLine(err.Error()), 120))
		status.flush(ctx, true)
		b.send(ctx, to, "Sorry, I couldn't start that turn: "+err.Error())
		return
	}

	var final string
	var runErr error
	for ev := range events {
		if line, ok := stepLine(ev, b.showThinking); ok {
			status.append(ctx, line)
		}
		switch ev.Type {
		case agent.EventDone:
			final = finalText(ev.Message)
		case agent.EventError:
			runErr = ev.Err
		}
	}
	status.flush(ctx, true)

	if runErr != nil {
		b.send(ctx, to, "Sorry, that turn failed: "+runErr.Error())
		return
	}
	if final == "" {
		b.send(ctx, to, "(the agent finished without any text to show)")
		return
	}
	b.send(ctx, to, final)
}

func (b *personalBridge) send(ctx context.Context, to jid.JID, text string) {
	if _, err := b.api.SendText(ctx, to, text, ""); err != nil {
		slog.Error("xmpp send failed", "to", to.String(), "err", err)
	}
}

type statusMessage struct {
	api    xmppAPI
	to     jid.JID
	minGap time.Duration
	now    func() time.Time

	id     string
	lines  []string
	shown  string
	lastAt time.Time
}

func (b *personalBridge) newStatus(to jid.JID) *statusMessage {
	return &statusMessage{api: b.api, to: to, minGap: b.correctionGap, now: b.now}
}

func (s *statusMessage) start(ctx context.Context) {
	id, err := s.api.SendText(ctx, s.to, "…", "")
	if err != nil {
		slog.Error("xmpp status create failed", "err", err)
	} else {
		s.id = id
	}
	s.lastAt = s.now()
}

func (s *statusMessage) append(ctx context.Context, line string) {
	s.lines = append(s.lines, line)
	s.flush(ctx, false)
}

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
	replaceID := s.id
	id, err := s.api.SendText(ctx, s.to, text, replaceID)
	if err != nil {
		slog.Error("xmpp status correction failed", "err", err)
	} else if s.id == "" {
		s.id = id
	}
	s.shown = text
	s.lastAt = s.now()
}
