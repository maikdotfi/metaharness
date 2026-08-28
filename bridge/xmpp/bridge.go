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

// maxOfflineAge is how stale a message the server queued while the bridge was
// offline may be and still be answered. One digest interval: an agent that
// speaks first is owed the reply typed while it was restarting, and nothing
// older is still the conversation.
const maxOfflineAge = 12 * time.Hour

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

	// Schedule is a prompt the bridge starts on its own, at fixed times of day.
	// The zero value schedules nothing, and every turn begins with a message.
	Schedule Schedule
}

// validate reports the configuration's problems and returns the account to log
// in as together with the people allowed to use it, by bare JID.
func (c Config) validate() (jid.JID, []jid.JID, error) {
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

	if err := c.Schedule.validate(); err != nil {
		return jid.JID{}, nil, err
	}

	var allowed []jid.JID
	seen := make(map[string]bool, len(c.AllowedJIDs))
	for _, raw := range c.AllowedJIDs {
		raw = strings.TrimSpace(raw)
		j, err := jid.Parse(raw)
		if err != nil || j.Localpart() == "" || j.Domainpart() == "" {
			if err == nil {
				err = errors.New("a user and domain are required")
			}
			return jid.JID{}, nil, fmt.Errorf("xmpp: invalid allowed JID %q: %w", raw, err)
		}
		if bare := j.Bare(); !seen[bare.String()] {
			seen[bare.String()] = true
			allowed = append(allowed, bare)
		}
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

// presenceBody is the presence the bridge broadcasts. The status line is what a
// client shows next to the account, so a running service is visible without
// asking it anything.
type presenceBody struct {
	stanza.Presence
	Status string `xml:"status,omitempty"`
}

// availablePresence announces the running service. Availability is the empty
// presence type, so the stanza carries no type at all.
func (b *personalBridge) availablePresence() presenceBody {
	return presenceBody{
		Presence: stanza.Presence{ID: newMessageID()},
		Status:   "on duty — send me a message",
	}
}

// MarshalXML writes the presence through the stanza package, which leaves out
// the addresses a broadcast does not have.
func (p presenceBody) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	var payload xml.TokenReader
	if p.Status != "" {
		payload = xmlstream.Wrap(
			xmlstream.Token(xml.CharData(p.Status)),
			xml.StartElement{Name: xml.Name{Local: "status"}},
		)
	}
	_, err := xmlstream.Copy(e, p.Presence.Wrap(payload))
	return err
}

// presenceReply answers an inbound presence stanza, or reports that there is
// nothing to answer. A subscription request from an allowed account is granted
// so their client can see the service running; anyone else is refused, which
// also keeps the agent off a stranger's contact list.
func (b *personalBridge) presenceReply(p stanza.Presence) (presenceBody, bool) {
	if p.Type != stanza.SubscribePresence {
		return presenceBody{}, false
	}
	from := p.From.Bare()
	reply := stanza.Presence{ID: newMessageID(), To: from, Type: stanza.UnsubscribedPresence}
	if b.allowed[from.String()] {
		reply.Type = stanza.SubscribedPresence
	}
	return presenceBody{Presence: reply}, true
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

	// schedule is the prompt the bridge starts on its own, and allowedJIDs the
	// bare JIDs it sends the answer to, having no incoming stanza to reply to.
	schedule    Schedule
	allowedJIDs []jid.JID

	correctionGap time.Duration
	now           func() time.Time
	after         func(time.Duration) <-chan time.Time

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
		allowed:       make(map[string]bool, len(allowed)),
		allowedJIDs:   allowed,
		showThinking:  cfg.ShowThinking,
		schedule:      cfg.Schedule,
		correctionGap: defaultCorrectionGap,
		now:           time.Now,
		after:         time.After,
	}
	for _, j := range allowed {
		b.allowed[j.String()] = true
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

	if err := s.Encode(ctx, b.availablePresence()); err != nil {
		return fmt.Errorf("xmpp: sending initial presence: %w", err)
	}

	incoming := newMessageQueue()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.Serve(b.incomingHandler(ctx, incoming))
	}()

	slog.Info("xmpp bridge started",
		"jid", s.LocalAddr().String(), "session", b.current.ID,
		"sandbox", b.sandboxName, "allowed_jids", len(b.allowed))

	return b.serve(ctx, incoming, serveDone)
}

// serve runs one turn at a time until the stream ends or ctx is cancelled. A
// scheduled slot is a fourth case rather than a goroutine, so a slot that comes
// due during a turn is served when that turn ends.
func (b *personalBridge) serve(ctx context.Context, incoming *messageQueue, serveDone <-chan error) error {
	for {
		var due <-chan time.Time
		if !b.schedule.zero() {
			now := b.now()
			if next := nextDue(now, b.schedule.at); !next.IsZero() {
				due = b.after(next.Sub(now))
			}
		}
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
		case <-due:
			b.runScheduled(ctx)
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

func (b *personalBridge) incomingHandler(ctx context.Context, incoming *messageQueue) mxmpp.Handler {
	return mxmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
		if p, ok := decodePresence(start); ok {
			reply, ok := b.presenceReply(p)
			if !ok {
				return nil
			}
			if err := t.Encode(reply); err != nil {
				slog.Warn("xmpp subscription answer failed", "to", reply.To.String(), "err", err)
			}
			return nil
		}
		msg, ok, err := decodeMessage(t, start, b.now())
		if err != nil {
			// One stanza the bridge cannot read is not a reason to drop the
			// stream and stop answering; the next one may be fine.
			slog.Warn("xmpp undecodable stanza ignored", "stanza", start.Name.Local, "err", err)
			return nil
		}
		if !ok {
			return nil
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

// decodePresence reads the attributes of an inbound presence stanza. Its
// payload holds nothing the bridge acts on, so it is left unread.
func decodePresence(start *xml.StartElement) (stanza.Presence, bool) {
	if start.Name.Local != "presence" ||
		(start.Name.Space != "" && start.Name.Space != stanza.NSClient) {
		return stanza.Presence{}, false
	}
	p, err := stanza.NewPresence(*start)
	if err != nil {
		slog.Warn("xmpp undecodable presence ignored", "err", err)
		return stanza.Presence{}, false
	}
	return p, true
}

func decodeMessage(r xml.TokenReader, start *xml.StartElement, now time.Time) (messageBody, bool, error) {
	if start.Name.Local != "message" ||
		(start.Name.Space != "" && start.Name.Space != stanza.NSClient) {
		return messageBody{}, false, nil
	}
	// The stream hands a handler the stanza's tokens with the start element
	// already read, so it goes back in front of them for the decoder.
	var msg messageBody
	d := xml.NewTokenDecoder(xmlstream.MultiReader(xmlstream.Token(*start), r))
	if err := d.Decode(&msg); err != nil && !errors.Is(err, io.EOF) {
		return messageBody{}, false, err
	}
	// A correction is the XMPP equivalent of an edited update. The original may
	// already be running, so treating the correction as another prompt could
	// repeat tool side effects.
	if msg.Type != stanza.ChatMessage || msg.Body == "" || msg.Replace != nil {
		return messageBody{}, false, nil
	}
	// A delayed message was queued while the bridge was offline. A recent one is
	// the answer to something the agent said unprompted, and worth having.
	if msg.Delay != nil && now.Sub(msg.Delay.Stamp) > maxOfflineAge {
		slog.Info("xmpp stale offline message ignored",
			"from", msg.From.String(), "stamp", msg.Delay.Stamp)
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
	if !b.schedule.zero() {
		fmt.Fprintf(&h, "\n\nI also start a turn on my own at %s, and send what comes of it here.",
			strings.Join(b.schedule.at, " and "))
	}
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
	b.runTurn(ctx, text, []jid.JID{to}, b.newStatus(to))
}

// runScheduled starts a turn nobody asked for. It replaces the current session
// unless the schedule continues it, and says nothing about having done so: the
// digest is the announcement, and /status shows the new id.
func (b *personalBridge) runScheduled(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.schedule.continuing {
		next, err := b.startSession()
		if err != nil {
			slog.Error("scheduled run could not start a fresh session", "err", err)
			return
		}
		b.closeCurrent()
		b.current = next
	}
	slog.Info("scheduled turn starting", "session", b.current.ID)
	b.runTurn(ctx, b.schedule.prompt, b.allowedJIDs, nil)
}

// runTurn runs one prompt and sends its answer to every recipient. A nil status
// is a scheduled turn: no progress trail, and no word when there is nothing to
// say. It expects b.mu held.
func (b *personalBridge) runTurn(ctx context.Context, text string, to []jid.JID, status *statusMessage) {
	sess := b.current
	sess.Messages = append(sess.Messages, model.NewUserMessage(text))
	sess.Status = agent.StatusActive

	status.start(ctx)

	events, err := b.agent.Run(ctx, sess)
	if err != nil {
		status.append(ctx, "❌ "+clip(oneLine(err.Error()), 120))
		status.flush(ctx, true)
		b.sendAll(ctx, to, "Sorry, I couldn't start that turn: "+err.Error())
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
		b.sendAll(ctx, to, "Sorry, that turn failed: "+runErr.Error())
		return
	}
	if final == "" {
		if status != nil {
			b.sendAll(ctx, to, "(the agent finished without any text to show)")
		}
		return
	}
	b.sendAll(ctx, to, final)
}

func (b *personalBridge) send(ctx context.Context, to jid.JID, text string) {
	if _, err := b.api.SendText(ctx, to, text, ""); err != nil {
		slog.Error("xmpp send failed", "to", to.String(), "err", err)
	}
}

func (b *personalBridge) sendAll(ctx context.Context, to []jid.JID, text string) {
	for _, j := range to {
		b.send(ctx, j, text)
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

// A nil statusMessage is a turn nobody is watching: every method is a no-op.
func (s *statusMessage) start(ctx context.Context) {
	if s == nil {
		return
	}
	id, err := s.api.SendText(ctx, s.to, "…", "")
	if err != nil {
		slog.Error("xmpp status create failed", "err", err)
	} else {
		s.id = id
	}
	s.lastAt = s.now()
}

func (s *statusMessage) append(ctx context.Context, line string) {
	if s == nil {
		return
	}
	s.lines = append(s.lines, line)
	s.flush(ctx, false)
}

func (s *statusMessage) flush(ctx context.Context, force bool) {
	if s == nil || len(s.lines) == 0 {
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
