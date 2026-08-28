package xmpp

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"mellium.im/xmlstream"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/testutils"
	"github.com/maikdotfi/metaharness/tools"
)

type sentMessage struct {
	to        jid.JID
	text      string
	replaceID string
	id        string
}

type fakeAPI struct {
	mu      sync.Mutex
	sends   []sentMessage
	sendErr error
}

func (f *fakeAPI) SendText(_ context.Context, to jid.JID, text, replaceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "msg_test_" + string(rune('a'+len(f.sends)))
	f.sends = append(f.sends, sentMessage{to: to, text: text, replaceID: replaceID, id: id})
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return id, nil
}

func (f *fakeAPI) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sends...)
}

func asstText(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

func asstToolCall(name, input string) fantasy.Message {
	return fantasy.Message{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.ToolCallPart{
			ToolCallID: "call-1", ToolName: name, Input: input,
		}},
	}
}

func newTestBridge(t *testing.T, m model.ModelClient, allowed ...string) (*personalBridge, *fakeAPI) {
	t.Helper()
	if len(allowed) == 0 {
		allowed = []string{"owner@example.org"}
	}
	a := agent.New("test system prompt",
		agent.WithModel(m),
		agent.WithTools(agent.Adapt(tools.Bash{})),
	)
	b, err := newBridge(Config{
		Username:    "agent@example.org",
		Password:    "secret",
		Agent:       a,
		Sandboxes:   &testutils.FakeSandboxes{},
		SandboxName: "work",
		Model:       "test-model",
		AllowedJIDs: allowed,
	})
	if err != nil {
		t.Fatalf("newBridge() error = %v", err)
	}
	api := &fakeAPI{}
	b.api = api
	b.correctionGap = 0
	return b, api
}

func incoming(from, body string) messageBody {
	return messageBody{
		Message: stanza.Message{From: jid.MustParse(from), Type: stanza.ChatMessage},
		Body:    body,
	}
}

func TestConfigValidation(t *testing.T) {
	a := agent.New("test", agent.WithModel(&testutils.ScriptedModel{}))
	valid := Config{
		Username:    "agent@example.org",
		Password:    "secret",
		Agent:       a,
		Sandboxes:   &testutils.FakeSandboxes{},
		SandboxName: "work",
		Model:       "test-model",
		AllowedJIDs: []string{"owner@example.org"},
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty username", func(c *Config) { c.Username = "" }, "Username"},
		{"domain is not an account", func(c *Config) { c.Username = "example.org" }, "Username"},
		{"empty password", func(c *Config) { c.Password = "" }, "Password"},
		{"nil agent", func(c *Config) { c.Agent = nil }, "Agent"},
		{"nil sandboxes", func(c *Config) { c.Sandboxes = nil }, "Sandboxes"},
		{"empty sandbox", func(c *Config) { c.SandboxName = "" }, "SandboxName"},
		{"empty model", func(c *Config) { c.Model = "" }, "Model"},
		{"no allowlist", func(c *Config) { c.AllowedJIDs = nil }, "allowed JID"},
		{"invalid allowed account", func(c *Config) { c.AllowedJIDs = []string{"example.org"} }, "allowed JID"},
		{"malformed schedule time", func(c *Config) { c.Schedule = Daily("digest", "07:30", "half seven") }, "half seven"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			_, err := newBridge(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newBridge() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestAllowedBareJIDAcceptsEveryResourceAndRepliesBare(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("the answer")}}
	b, api := newTestBridge(t, m, "owner@example.org/laptop")

	b.handleMessage(context.Background(), incoming("owner@example.org/phone", "review this"))

	if len(m.Calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(m.Calls))
	}
	if got := len(b.current.Messages); got != 2 {
		t.Fatalf("transcript length = %d, want 2", got)
	}
	msgs := api.messages()
	if len(msgs) < 2 {
		t.Fatalf("sent messages = %#v, want progress and answer", msgs)
	}
	for _, msg := range msgs {
		if got := msg.to.String(); got != "owner@example.org" {
			t.Errorf("reply target = %q, want bare owner JID", got)
		}
	}
	if got := msgs[len(msgs)-1].text; got != "the answer" {
		t.Errorf("final reply = %q, want %q", got, "the answer")
	}
}

func TestUnauthorizedJIDNeverInvokesAgent(t *testing.T) {
	m := &testutils.ScriptedModel{}
	b, api := newTestBridge(t, m)

	b.handleMessage(context.Background(), incoming("stranger@example.org/phone", "let me in"))

	if len(m.Calls) != 0 {
		t.Fatalf("model was called %d times", len(m.Calls))
	}
	if len(b.current.Messages) != 0 {
		t.Fatalf("unauthorized message mutated transcript: %#v", b.current.Messages)
	}
	if len(api.messages()) != 0 {
		t.Fatalf("bridge replied to unauthorized sender: %#v", api.messages())
	}
}

func TestCommandsUseOneCurrentSession(t *testing.T) {
	m := &testutils.ScriptedModel{}
	b, api := newTestBridge(t, m)
	ctx := context.Background()
	first := b.current.ID

	b.handleMessage(ctx, incoming("owner@example.org/phone", "/status"))
	if got := api.messages()[0].text; !strings.Contains(got, first) {
		t.Errorf("/status = %q, want session %q", got, first)
	}
	b.handleMessage(ctx, incoming("owner@example.org/laptop", "/new"))
	if b.current.ID == first {
		t.Fatalf("/new retained session %q", first)
	}
	if len(m.Calls) != 0 || len(b.current.Messages) != 0 {
		t.Fatal("commands reached the model or transcript")
	}
}

func TestProgressCorrectsOneMessageAndFinalIsSeparate(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{
		asstToolCall("bash", `{"cmd":"ls -la"}`),
		asstText("all reviewed"),
	}}
	b, api := newTestBridge(t, m)

	b.handleMessage(context.Background(), incoming("owner@example.org/phone", "review"))

	msgs := api.messages()
	if len(msgs) < 3 {
		t.Fatalf("sent %d messages, want placeholder, correction, and answer: %#v", len(msgs), msgs)
	}
	original := msgs[0]
	if original.text != "…" || original.replaceID != "" {
		t.Fatalf("first progress message = %#v", original)
	}
	var corrected bool
	for _, msg := range msgs[1 : len(msgs)-1] {
		if msg.replaceID == original.id && strings.Contains(msg.text, "🔧 bash: ls -la") && strings.Contains(msg.text, "✅ done") {
			corrected = true
		}
		if strings.Contains(msg.text, "all reviewed") {
			t.Fatalf("answer leaked into progress correction: %#v", msg)
		}
	}
	if !corrected {
		t.Fatalf("no complete correction of original %q in %#v", original.id, msgs)
	}
	final := msgs[len(msgs)-1]
	if final.text != "all reviewed" || final.replaceID != "" {
		t.Fatalf("final message = %#v, want a separate answer", final)
	}
}

// The stream hands a handler the stanza's own tokens: the start element is
// already consumed, and the reader ends after the stanza's end element.
func handlerTokens(t *testing.T, raw string) (xml.TokenReader, *xml.StartElement) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil {
		t.Fatal(err)
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		t.Fatalf("first token = %T, want a start element", tok)
	}
	return xmlstream.InnerElement(d), &start
}

func TestDecodeMessageAcceptsOnlyDirectChatBodies(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		xml  string
		ok   bool
	}{
		{"chat body", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>hello</body></message>`, true},
		{"normal", `<message xmlns="jabber:client" from="owner@example.org/p" type="normal"><body>hello</body></message>`, false},
		{"empty body", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body></body></message>`, false},
		{"edited message", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>changed</body><replace xmlns="urn:xmpp:message-correct:0" id="old"/></message>`, false},
		{"a reply queued while the bridge restarted", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>hello</body><delay xmlns="urn:xmpp:delay" from="example.org" stamp="2026-08-24T10:00:00Z"/></message>`, true},
		{"a message older than a digest interval", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>old task</body><delay xmlns="urn:xmpp:delay" from="example.org" stamp="2026-08-23T10:00:00Z"/></message>`, false},
		{"presence", `<presence xmlns="jabber:client" from="owner@example.org/p"/>`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, start := handlerTokens(t, tc.xml)
			msg, ok, err := decodeMessage(r, start, now)
			if err != nil {
				t.Fatalf("decodeMessage() error = %v", err)
			}
			if ok != tc.ok {
				t.Fatalf("decodeMessage() ok = %v, want %v (msg %#v)", ok, tc.ok, msg)
			}
			if !ok {
				return
			}
			if msg.Body != "hello" {
				t.Errorf("body = %q, want hello", msg.Body)
			}
			if got := msg.From.String(); got != "owner@example.org/p" {
				t.Errorf("from = %q, want owner@example.org/p", got)
			}
		})
	}
}

func TestMessageQueuePreservesArrivalOrder(t *testing.T) {
	q := newMessageQueue()
	q.push(incoming("owner@example.org/a", "first"))
	q.push(incoming("owner@example.org/b", "second"))

	first, ok := q.pop()
	if !ok || first.Body != "first" {
		t.Fatalf("first pop = %#v, %v", first, ok)
	}
	second, ok := q.pop()
	if !ok || second.Body != "second" {
		t.Fatalf("second pop = %#v, %v", second, ok)
	}
	if _, ok := q.pop(); ok {
		t.Fatal("empty queue produced another message")
	}
}

func TestOutgoingCorrectionHasNewIDAndReplacePayload(t *testing.T) {
	msg := messageBody{
		Message: stanza.Message{
			ID: "new-id", To: jid.MustParse("owner@example.org"), Type: stanza.ChatMessage,
		},
		Body:    "updated progress",
		Replace: &replacement{ID: "original-id"},
	}
	b, err := xml.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	xmlText := string(b)
	for _, want := range []string{
		`id="new-id"`,
		`type="chat"`,
		`<body>updated progress</body>`,
		`xmlns="urn:xmpp:message-correct:0"`,
		`id="original-id"`,
	} {
		if !strings.Contains(xmlText, want) {
			t.Errorf("marshaled message %q does not contain %q", xmlText, want)
		}
	}
}

func TestFirstSandboxFailureIsReturned(t *testing.T) {
	a := agent.New("test", agent.WithModel(&testutils.ScriptedModel{}))
	_, err := newBridge(Config{
		Username:    "agent@example.org",
		Password:    "secret",
		Agent:       a,
		Sandboxes:   &testutils.FakeSandboxes{OpenErr: errors.New("no backend")},
		SandboxName: "work",
		Model:       "test-model",
		AllowedJIDs: []string{"owner@example.org"},
	})
	if err == nil || !strings.Contains(err.Error(), "no backend") {
		t.Fatalf("newBridge() error = %v, want sandbox failure", err)
	}
}

// gatedModel holds its first reply until the gate opens, so a test can make a
// slot come due while a typed turn is still running.
type gatedModel struct {
	gate chan struct{}

	mu      sync.Mutex
	prompts []string
}

func (m *gatedModel) Generate(_ context.Context, req model.ModelRequest) (fantasy.Message, fantasy.Usage, error) {
	last := req.Messages[len(req.Messages)-1]
	m.mu.Lock()
	first := len(m.prompts) == 0
	m.prompts = append(m.prompts, model.TextParts(&last)[0].Text)
	m.mu.Unlock()
	if first {
		<-m.gate
	}
	return asstText("done"), fantasy.Usage{}, nil
}

type fakeTimer struct {
	waits chan time.Duration
	fires chan time.Time
}

func newFakeTimer() *fakeTimer {
	return &fakeTimer{waits: make(chan time.Duration, 8), fires: make(chan time.Time, 1)}
}

func (f *fakeTimer) after(d time.Duration) <-chan time.Time {
	f.waits <- d
	return f.fires
}

// serving runs the bridge's loop with a replaceable timer, and returns the
// timer, the incoming queue, and a wait that stops the loop.
func serving(t *testing.T, b *personalBridge) (*fakeTimer, *messageQueue, func()) {
	t.Helper()
	timer := newFakeTimer()
	b.after = timer.after
	queue := newMessageQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.serve(ctx, queue, make(chan error, 1)) }()
	return timer, queue, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("serve did not return after cancellation")
		}
	}
}

// awaitSlot waits for the loop to ask how long until the next slot, which is
// also how a test knows the turn before it finished.
func (f *fakeTimer) awaitSlot(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-f.waits:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("the loop never waited for a slot")
		return 0
	}
}

func newScheduledBridge(t *testing.T, m model.ModelClient, s Schedule, allowed ...string) (*personalBridge, *fakeAPI) {
	t.Helper()
	b, api := newTestBridge(t, m, allowed...)
	b.schedule = s
	return b, api
}

func TestScheduledSlotRunsThePromptAndAnswersTheBareJID(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("two new episodes")}}
	b, api := newScheduledBridge(t, m, Daily("what is new?", "07:30"), "owner@example.org/laptop")

	timer, _, stop := serving(t, b)
	timer.awaitSlot(t)
	timer.fires <- time.Now()
	timer.awaitSlot(t)
	stop()

	if len(m.Calls) != 1 {
		t.Fatalf("model calls = %d, want the schedule's prompt to have run once", len(m.Calls))
	}
	msgs := api.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want only the digest: %#v", len(msgs), msgs)
	}
	if got := msgs[0].text; got != "two new episodes" {
		t.Errorf("digest = %q, want %q", got, "two new episodes")
	}
	if got := msgs[0].to.String(); got != "owner@example.org" {
		t.Errorf("digest went to %q, want the bare allowed JID", got)
	}
}

func TestScheduledTurnShowsNoProgressTrail(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{
		asstToolCall("bash", `{"cmd":"ls"}`),
		asstText("all quiet, one change"),
	}}
	b, api := newScheduledBridge(t, m, Daily("what is new?", "07:30"))

	timer, _, stop := serving(t, b)
	timer.awaitSlot(t)
	timer.fires <- time.Now()
	timer.awaitSlot(t)
	stop()

	msgs := api.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want exactly the answer: %#v", len(msgs), msgs)
	}
	if msgs[0].replaceID != "" {
		t.Errorf("scheduled answer corrected %q, want a plain message", msgs[0].replaceID)
	}
}

func TestScheduledTurnWithNothingToSaySendsNothing(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("")}}
	b, api := newScheduledBridge(t, m, Daily("what is new?", "07:30"))

	timer, _, stop := serving(t, b)
	timer.awaitSlot(t)
	timer.fires <- time.Now()
	timer.awaitSlot(t)
	stop()

	if msgs := api.messages(); len(msgs) != 0 {
		t.Fatalf("silent run sent %#v", msgs)
	}
}

func TestScheduledRunStartsAFreshSessionInTheSameSandbox(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("morning")}}
	b, _ := newScheduledBridge(t, m, Daily("what is new?", "07:30"))
	before, sandbox := b.current.ID, b.current.SandboxName()

	timer, _, stop := serving(t, b)
	timer.awaitSlot(t)
	timer.fires <- time.Now()
	timer.awaitSlot(t)
	stop()

	if b.current.ID == before {
		t.Errorf("session id = %q, want a fresh session", b.current.ID)
	}
	if got := b.current.SandboxName(); got != sandbox {
		t.Errorf("sandbox = %q, want the same %q", got, sandbox)
	}
	if got := len(b.current.Messages); got != 2 {
		t.Errorf("transcript length = %d, want only the scheduled turn", got)
	}
}

func TestContinuingScheduleKeepsTheSession(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("hi"), asstText("morning")}}
	b, _ := newScheduledBridge(t, m, Daily("what is new?", "07:30").Continuing())
	b.handleMessage(context.Background(), incoming("owner@example.org/phone", "hi"))
	before := b.current.ID

	timer, _, stop := serving(t, b)
	timer.awaitSlot(t)
	timer.fires <- time.Now()
	timer.awaitSlot(t)
	stop()

	if b.current.ID != before {
		t.Errorf("session id = %q, want the same %q", b.current.ID, before)
	}
	if got := len(b.current.Messages); got != 4 {
		t.Errorf("transcript length = %d, want both turns", got)
	}
}

func TestScheduledTurnWaitsForTheTypedOneInFlight(t *testing.T) {
	m := &gatedModel{gate: make(chan struct{})}
	b, api := newScheduledBridge(t, m, Daily("what is new?", "07:30"))

	timer, queue, stop := serving(t, b)
	timer.awaitSlot(t)
	queue.push(incoming("owner@example.org/phone", "review this"))
	timer.fires <- time.Now() // comes due while the typed turn is blocked
	close(m.gate)
	timer.awaitSlot(t)
	timer.awaitSlot(t)
	stop()

	m.mu.Lock()
	prompts := append([]string(nil), m.prompts...)
	m.mu.Unlock()
	if len(prompts) != 2 || prompts[0] != "review this" || prompts[1] != "what is new?" {
		t.Fatalf("prompts = %#v, want the typed turn then the scheduled one", prompts)
	}
	msgs := api.messages()
	if len(msgs) < 2 {
		t.Fatalf("sent %#v, want the typed turn's trail and the digest", msgs)
	}
	if got := msgs[len(msgs)-1].text; got != "done" || msgs[len(msgs)-1].replaceID != "" {
		t.Fatalf("last message = %#v, want the scheduled answer after the typed turn", msgs[len(msgs)-1])
	}
}

func TestNoScheduleNeverWaitsForASlot(t *testing.T) {
	m := &testutils.ScriptedModel{Replies: []fantasy.Message{asstText("the answer")}}
	b, api := newTestBridge(t, m)

	timer, queue, stop := serving(t, b)
	queue.push(incoming("owner@example.org/phone", "review this"))
	waitFor(t, func() bool { return len(api.messages()) >= 2 })
	stop()

	select {
	case d := <-timer.waits:
		t.Fatalf("an empty schedule waited %s for a slot", d)
	default:
	}
	if got := api.messages()[len(api.messages())-1].text; got != "the answer" {
		t.Errorf("final reply = %q, want the typed answer", got)
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the bridge")
}

func TestHelpMentionsTheSchedule(t *testing.T) {
	m := &testutils.ScriptedModel{}
	b, api := newScheduledBridge(t, m, Daily("what is new?", "07:30", "18:30"))

	b.handleMessage(context.Background(), incoming("owner@example.org/phone", "/help"))

	help := api.messages()[0].text
	for _, want := range []string{"07:30", "18:30"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help = %q, want it to name the slot %q", help, want)
		}
	}

	quiet, quietAPI := newTestBridge(t, m)
	quiet.handleMessage(context.Background(), incoming("owner@example.org/phone", "/help"))
	if got := quietAPI.messages()[0].text; strings.Contains(got, "on my own") {
		t.Errorf("/help without a schedule = %q, want no mention of one", got)
	}
}

// encoderStub is the reader-plus-writer the stream hands a handler. It records
// the stanzas the handler answers with, in place of the real stream.
type encoderStub struct {
	xml.TokenReader
	encoded []any
}

func (*encoderStub) EncodeToken(xml.Token) error { return nil }

func (e *encoderStub) Encode(v any) error { e.encoded = append(e.encoded, v); return nil }

func (e *encoderStub) EncodeElement(v any, _ xml.StartElement) error { return e.Encode(v) }

func TestStreamSurvivesAStanzaItCannotDecode(t *testing.T) {
	now := func() time.Time { return time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC) }
	q := newMessageQueue()
	r, start := handlerTokens(t, `<message xmlns="jabber:client" from="not a jid" type="chat"><body>hello</body></message>`)

	b, _ := newTestBridge(t, &testutils.ScriptedModel{})
	b.now = now
	err := b.incomingHandler(context.Background(), q).HandleXMPP(&encoderStub{TokenReader: r}, start)
	if err != nil {
		t.Fatalf("handler returned %v: one undecodable stanza ended the stream", err)
	}
	if msg, ok := q.pop(); ok {
		t.Fatalf("undecodable stanza was queued as a prompt: %#v", msg)
	}
}

func TestAnnouncedPresenceSaysTheServiceIsRunning(t *testing.T) {
	b, _ := newTestBridge(t, &testutils.ScriptedModel{})

	out, err := xml.Marshal(b.availablePresence())
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "type=") {
		t.Errorf("presence %q has a type: availability is the empty one", got)
	}
	for _, unwanted := range []string{`to=""`, `from=""`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("presence %q carries %s: a broadcast is addressed to nobody", got, unwanted)
		}
	}
	if !strings.Contains(got, "<status>") || strings.Contains(got, "<status></status>") {
		t.Errorf("presence %q carries no status line saying the service is running", got)
	}
}

func TestSubscriptionRequestsAreAnsweredByTheAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		xmlText string
		want    stanza.PresenceType
		to      string
	}{
		{
			"a request from an allowed account",
			`<presence xmlns="jabber:client" from="owner@example.org/phone" type="subscribe"/>`,
			stanza.SubscribedPresence, "owner@example.org",
		},
		{
			"a request from a stranger",
			`<presence xmlns="jabber:client" from="spam@elsewhere.org/x" type="subscribe"/>`,
			stanza.UnsubscribedPresence, "spam@elsewhere.org",
		},
		{
			"a contact coming online",
			`<presence xmlns="jabber:client" from="owner@example.org/phone"/>`,
			"", "",
		},
		{
			"a contact unsubscribing",
			`<presence xmlns="jabber:client" from="owner@example.org/phone" type="unsubscribe"/>`,
			"", "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestBridge(t, &testutils.ScriptedModel{})
			q := newMessageQueue()
			r, start := handlerTokens(t, tc.xmlText)
			enc := &encoderStub{TokenReader: r}

			if err := b.incomingHandler(context.Background(), q).HandleXMPP(enc, start); err != nil {
				t.Fatalf("handler returned %v", err)
			}
			if msg, ok := q.pop(); ok {
				t.Fatalf("presence was queued as a prompt: %#v", msg)
			}
			if tc.to == "" {
				if len(enc.encoded) != 0 {
					t.Fatalf("bridge answered with %#v, want silence", enc.encoded)
				}
				return
			}
			if len(enc.encoded) != 1 {
				t.Fatalf("bridge sent %d stanzas, want one answer", len(enc.encoded))
			}
			reply, ok := enc.encoded[0].(presenceBody)
			if !ok {
				t.Fatalf("answer = %#v, want a presence", enc.encoded[0])
			}
			if reply.Type != tc.want {
				t.Errorf("answer type = %q, want %q", reply.Type, tc.want)
			}
			if got := reply.To.String(); got != tc.to {
				t.Errorf("answer to = %q, want the bare %q", got, tc.to)
			}
		})
	}
}
