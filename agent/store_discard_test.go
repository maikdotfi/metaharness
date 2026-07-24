package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maikdotfi/metaharness/agent"
)

func TestDiscardStore(t *testing.T) {
	var store agent.SessionStore = agent.DiscardStore{}

	if err := store.Save(context.Background(), &agent.Session{ID: "discarded"}); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}

	sess, err := store.Load(context.Background(), "discarded")
	if sess != nil {
		t.Fatalf("Load() session = %#v, want nil", sess)
	}
	if !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("Load() error = %v, want ErrNotFound", err)
	}
}

func TestNewUsesDiscardStoreByDefault(t *testing.T) {
	a := agent.New("test")

	if _, ok := a.Store.(agent.DiscardStore); !ok {
		t.Fatalf("default store = %T, want agent.DiscardStore", a.Store)
	}
}
