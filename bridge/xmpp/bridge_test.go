package xmpp

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
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

func TestDecodeMessageAcceptsOnlyDirectChatBodies(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		ok   bool
	}{
		{"chat body", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>hello</body></message>`, true},
		{"normal", `<message xmlns="jabber:client" from="owner@example.org/p" type="normal"><body>hello</body></message>`, false},
		{"empty body", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body></body></message>`, false},
		{"edited message", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>changed</body><replace xmlns="urn:xmpp:message-correct:0" id="old"/></message>`, false},
		{"offline message", `<message xmlns="jabber:client" from="owner@example.org/p" type="chat"><body>old task</body><delay xmlns="urn:xmpp:delay" from="example.org" stamp="2026-08-24T10:00:00Z"/></message>`, false},
		{"presence", `<presence xmlns="jabber:client" from="owner@example.org/p"/>`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := xml.NewDecoder(strings.NewReader(tc.xml))
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			start := tok.(xml.StartElement)
			msg, ok, err := decodeMessage(d, &start)
			if err != nil {
				t.Fatalf("decodeMessage() error = %v", err)
			}
			if ok != tc.ok {
				t.Fatalf("decodeMessage() ok = %v, want %v (msg %#v)", ok, tc.ok, msg)
			}
			if ok && msg.Body != "hello" {
				t.Errorf("body = %q, want hello", msg.Body)
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
