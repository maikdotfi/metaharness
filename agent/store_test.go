package agent_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/testutils"
)

// metaLine reads the first line of a session file and decodes it as far as the
// sandbox field, which it keeps raw so a test can see the shape that was
// written rather than only the value.
func metaLine(t *testing.T, dir, id string) json.RawMessage {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatalf("opening the session file: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("the session file has no meta line: %v", sc.Err())
	}
	var rec struct {
		Kind string `json:"kind"`
		Meta struct {
			Sandbox json.RawMessage `json:"sandbox"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("decoding the meta line: %v", err)
	}
	if rec.Kind != "meta" {
		t.Fatalf("first record kind = %q, want %q", rec.Kind, "meta")
	}
	return rec.Meta.Sandbox
}

// TestSaveRecordsOnlyTheSandboxName pins what survives a session: the name, and
// nothing about where sandboxes live or what they were made from. Image and
// backend are this process's configuration, and the next one is free to differ.
func TestSaveRecordsOnlyTheSandboxName(t *testing.T) {
	dir := t.TempDir()
	store, err := agent.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	sess := testutils.UserSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "hi")
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var name string
	if err := json.Unmarshal(metaLine(t, dir, "s1"), &name); err != nil {
		t.Fatalf("the sandbox was not recorded as a bare name: %v", err)
	}
	if name != "work" {
		t.Errorf("recorded sandbox = %q, want %q", name, "work")
	}
}

// TestLoadRestoresTheSandboxName covers the round trip the resuming application
// depends on.
func TestLoadRestoresTheSandboxName(t *testing.T) {
	ctx := context.Background()
	store, err := agent.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	sess := testutils.UserSession("s1", "fake-model", &testutils.FakeSandbox{SandboxName: "work"}, "hi")
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SandboxName() != "work" {
		t.Errorf("SandboxName() = %q, want %q", got.SandboxName(), "work")
	}
	if len(got.Messages) != 1 {
		t.Errorf("loaded %d messages, want 1", len(got.Messages))
	}
}

// TestLoadReadsLegacySandboxRecord keeps sessions written before the sandbox was
// recorded as a name readable: their sandbox was an object with a name and an
// image, and the name is the part that still means anything.
func TestLoadReadsLegacySandboxRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("writing the legacy session file: %v", err)
	}
	enc := json.NewEncoder(f)
	records := []map[string]any{
		{"kind": "meta", "meta": map[string]any{
			"id":      "legacy",
			"model":   "old-model",
			"status":  string(agent.StatusActive),
			"sandbox": map[string]any{"name": "work", "image": "golang:1.26"},
		}},
		{"kind": "message", "message": fantasy.NewUserMessage("hello")},
	}
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			t.Fatalf("encoding a legacy record: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the legacy session file: %v", err)
	}

	store, err := agent.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	got, err := store.Load(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SandboxName() != "work" {
		t.Errorf("SandboxName() = %q, want %q from the legacy record", got.SandboxName(), "work")
	}
	if got.Model != "old-model" {
		t.Errorf("Model = %q, want %q", got.Model, "old-model")
	}
	if len(got.Messages) != 1 {
		t.Errorf("loaded %d messages, want 1", len(got.Messages))
	}
}
