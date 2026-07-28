package testutils

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/maikdotfi/metaharness/agent"
)

// SessionStoreFactory creates an isolated store for one test.
type SessionStoreFactory func(t *testing.T) agent.SessionStore

// RunSessionStoreSuite checks the behavior shared by durable session stores.
func RunSessionStoreSuite(t *testing.T, newStore SessionStoreFactory) {
	t.Helper()

	t.Run("round trip", func(t *testing.T) {
		store := newStore(t)
		want := richSession("round-trip")

		if err := store.Save(context.Background(), want); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := store.Load(context.Background(), want.ID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Load() = %#v, want %#v", got, want)
		}
	})

	t.Run("upsert", func(t *testing.T) {
		store := newStore(t)
		first := richSession("same-id")
		if err := store.Save(context.Background(), first); err != nil {
			t.Fatalf("first Save() error = %v", err)
		}

		want := richSession("same-id")
		want.Model = "replacement-model"
		want.Status = agent.StatusFailed
		want.Messages = append(want.Messages, AssistantText("replacement"))
		if err := store.Save(context.Background(), want); err != nil {
			t.Fatalf("second Save() error = %v", err)
		}

		got, err := store.Load(context.Background(), want.ID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Load() after upsert = %#v, want %#v", got, want)
		}
	})

	t.Run("not found", func(t *testing.T) {
		store := newStore(t)
		got, err := store.Load(context.Background(), "missing")
		if got != nil {
			t.Fatalf("Load() = %#v, want nil", got)
		}
		if !errors.Is(err, agent.ErrNotFound) {
			t.Fatalf("Load() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("large tool result", func(t *testing.T) {
		store := newStore(t)
		want := richSession("large")
		want.Messages = append(want.Messages, fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "large-result",
				Output: fantasy.ToolResultOutputContentText{
					Text: strings.Repeat("large output ", 100_000),
				},
			}},
		})

		if err := store.Save(context.Background(), want); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := store.Load(context.Background(), want.ID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("large tool result did not survive round trip")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.Save(ctx, richSession("cancelled")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Save() error = %v, want context.Canceled", err)
		}
		got, err := store.Load(ctx, "cancelled")
		if got != nil {
			t.Fatalf("Load() = %#v, want nil", got)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want context.Canceled", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		store := newStore(t)
		lister, ok := store.(agent.SessionLister)
		if !ok {
			t.Fatalf("%T does not implement agent.SessionLister", store)
		}

		older := richSession("older")
		if err := store.Save(context.Background(), older); err != nil {
			t.Fatalf("Save(older) error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		newer := richSession("newer")
		newer.Messages = append(newer.Messages, AssistantText("one more"))
		if err := store.Save(context.Background(), newer); err != nil {
			t.Fatalf("Save(newer) error = %v", err)
		}

		got, err := lister.List(context.Background(), 1)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List() returned %d items, want 1", len(got))
		}
		want := newer
		if got[0].ID != want.ID || got[0].Model != want.Model ||
			got[0].Status != want.Status || got[0].Messages != len(want.Messages) ||
			!reflect.DeepEqual(got[0].Usage, want.Usage) || got[0].UpdatedAt.IsZero() {
			t.Fatalf("List()[0] = %#v, want metadata for %#v", got[0], want)
		}
	})
}

func richSession(id string) *agent.Session {
	return &agent.Session{
		ID:       id,
		Model:    "test-model",
		Status:   agent.StatusCompleted,
		Messages: []fantasy.Message{fantasy.NewUserMessage("hello"), AssistantText("hi")},
		Usage: fantasy.Usage{
			InputTokens:         11,
			OutputTokens:        7,
			TotalTokens:         18,
			ReasoningTokens:     2,
			CacheCreationTokens: 3,
			CacheReadTokens:     4,
		},
		Sandbox: agent.SandboxSpec{Image: "golang:latest"},
	}
}
