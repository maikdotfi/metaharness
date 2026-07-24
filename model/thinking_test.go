package model

import (
	"testing"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

func TestThinkingCallOptionsAnthropicUsesEffort(t *testing.T) {
	// Anthropic thinking must go through output_config.effort (adaptive
	// thinking), never the deprecated token budget.
	opts, minOutput := (&Thinking{}).callOptions(ProviderAnthropic)
	po, ok := opts[anthropic.Name].(*anthropic.ProviderOptions)
	if !ok || po.Effort == nil {
		t.Fatalf("expected anthropic effort, got %#v", opts[anthropic.Name])
	}
	if *po.Effort != anthropic.EffortMedium {
		t.Fatalf("effort = %q, want default %q", *po.Effort, anthropic.EffortMedium)
	}
	if po.Thinking != nil {
		t.Fatal("must not set a (deprecated) token budget")
	}
	if minOutput <= 4096 {
		t.Fatalf("min output %d should exceed the provider default", minOutput)
	}
}

func TestThinkingCallOptionsEffortPassThrough(t *testing.T) {
	// Effort passes through unchanged rather than being collapsed to a
	// three-value subset, so xhigh survives.
	opts, _ := (&Thinking{Effort: EffortXHigh}).callOptions(ProviderOpenAI)
	po, ok := opts[openai.Name].(*openai.ProviderOptions)
	if !ok || po.ReasoningEffort == nil {
		t.Fatalf("expected openai reasoning effort, got %#v", opts[openai.Name])
	}
	if *po.ReasoningEffort != openai.ReasoningEffortXHigh {
		t.Fatalf("effort = %q, want %q", *po.ReasoningEffort, openai.ReasoningEffortXHigh)
	}
}

func TestThinkingCallOptionsGoogleLevel(t *testing.T) {
	opts, _ := (&Thinking{Effort: EffortLow}).callOptions(ProviderGoogle)
	po, ok := opts[google.Name].(*google.ProviderOptions)
	if !ok || po.ThinkingConfig == nil || po.ThinkingConfig.ThinkingLevel == nil {
		t.Fatalf("expected google thinking level, got %#v", opts[google.Name])
	}
	if *po.ThinkingConfig.ThinkingLevel != google.ThinkingLevelLow {
		t.Fatalf("level = %q, want %q", *po.ThinkingConfig.ThinkingLevel, google.ThinkingLevelLow)
	}
	if po.ThinkingConfig.IncludeThoughts == nil || !*po.ThinkingConfig.IncludeThoughts {
		t.Fatal("expected IncludeThoughts true so thinking is returned")
	}
}

func TestNewEnablesThinking(t *testing.T) {
	m, err := New(Config{Provider: ProviderAnthropic, Thinking: &Thinking{}})
	if err != nil {
		t.Fatal(err)
	}
	fm, ok := m.(*FantasyModel)
	if !ok {
		t.Fatalf("expected *FantasyModel, got %T", m)
	}
	if fm.thinkingOpts == nil {
		t.Fatal("expected thinking options to be set on the model")
	}
}
