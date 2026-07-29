package agent_test

import (
	"reflect"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// TestRecordCarriesTheSandboxName is the seam a store outside package agent
// needs. The sandbox name lives in an unexported field, so without a record
// there is no way for such a store to write the name down — which is how the
// Turso store came to lose it.
func TestRecordCarriesTheSandboxName(t *testing.T) {
	sess := testutils.UserSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "hi")

	if got := sess.Record().Sandbox; got != "work" {
		t.Errorf("Record().Sandbox = %q, want %q", got, "work")
	}
}

// TestRecordRoundTripsASession pins what survives a store: everything a session
// is, apart from the live sandbox handle, which no store can write down.
func TestRecordRoundTripsASession(t *testing.T) {
	sess := testutils.UserSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "hi")
	sess.Status = agent.StatusCompleted
	sess.Usage = fantasy.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}
	sess.Messages = append(sess.Messages, testutils.AssistantText("hello"))

	got := sess.Record().Session()

	if got.ID != sess.ID {
		t.Errorf("ID = %q, want %q", got.ID, sess.ID)
	}
	if got.Model != sess.Model {
		t.Errorf("Model = %q, want %q", got.Model, sess.Model)
	}
	if got.Status != sess.Status {
		t.Errorf("Status = %q, want %q", got.Status, sess.Status)
	}
	if !reflect.DeepEqual(got.Usage, sess.Usage) {
		t.Errorf("Usage = %#v, want %#v", got.Usage, sess.Usage)
	}
	if !reflect.DeepEqual(got.Messages, sess.Messages) {
		t.Errorf("the transcript did not survive the record")
	}
	if got.SandboxName() != "work" {
		t.Errorf("SandboxName() = %q, want %q", got.SandboxName(), "work")
	}
	if got.Sandbox() != nil {
		t.Error("a recorded session came back holding a live sandbox handle")
	}
}

// TestSessionWithoutMessagesStaysWithoutMessages keeps an empty session from
// becoming an empty-but-allocated one, so a store round trip compares equal to
// the session it was given.
func TestSessionWithoutMessagesStaysWithoutMessages(t *testing.T) {
	rec := agent.NewSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}).Record()

	if got := rec.Session().Messages; got != nil {
		t.Errorf("Messages = %#v, want nil", got)
	}
}

// TestEachRestoredSessionOwnsItsTranscript stops two sessions restored from one
// record from writing over each other's turns. A transcript that is still shared
// has been borrowed, not restored.
func TestEachRestoredSessionOwnsItsTranscript(t *testing.T) {
	// Room to append without reallocating is what makes sharing observable: a
	// store hands out records built by whatever slice its Load produced.
	messages := make([]fantasy.Message, 0, 4)
	messages = append(messages, fantasy.NewUserMessage("hi"))
	rec := agent.SessionRecord{ID: "s1", Model: "fake-model", Sandbox: "work", Messages: messages}

	first, second := rec.Session(), rec.Session()
	first.Messages = append(first.Messages, testutils.AssistantText("first"))
	second.Messages = append(second.Messages, testutils.AssistantText("second"))

	if !reflect.DeepEqual(first.Messages[1], testutils.AssistantText("first")) {
		t.Error("appending to one restored session overwrote another's turn")
	}
}
