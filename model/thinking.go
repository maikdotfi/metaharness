package model

import (
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

// Effort is a provider-agnostic reasoning effort level. It is the single knob
// for thinking depth across providers: rather than a fixed token budget (which
// Anthropic has deprecated and now rejects on current models), every provider
// selects how hard to think from an effort level.
//
// Not every level is valid for every provider; the comments note where each
// applies. Unsupported values are rejected by the provider rather than silently
// downgraded.
type Effort string

const (
	EffortNone    Effort = "none"    // OpenAI
	EffortMinimal Effort = "minimal" // OpenAI, Google (maps to MINIMAL)
	EffortLow     Effort = "low"     // Anthropic, OpenAI, Google
	EffortMedium  Effort = "medium"  // Anthropic, OpenAI, Google
	EffortHigh    Effort = "high"    // Anthropic, OpenAI, Google
	EffortXHigh   Effort = "xhigh"   // Anthropic, OpenAI
	EffortMax     Effort = "max"     // Anthropic
)

// thinkingMaxOutputTokens is the minimum output-token allowance a request uses
// when thinking is on. Reasoning shares the output budget with the answer, and
// providers default this low (fantasy's Anthropic default is 4096), so we raise
// it enough that thinking does not truncate the reply. It stays within the
// non-streaming range to avoid HTTP timeouts on the agent's non-streaming call.
const thinkingMaxOutputTokens int64 = 16000

// Thinking enables optional extended thinking (a.k.a. reasoning) output. Set it
// on Config.Thinking so every request from the resulting model asks the
// provider to think before it answers.
//
// Thinking depth is controlled by Effort, which maps to each provider's native
// mechanism:
//   - Anthropic: output_config.effort with adaptive thinking.
//   - OpenAI: reasoning_effort.
//   - Google: thinking_config.thinking_level.
type Thinking struct {
	// Effort selects how hard the model thinks. Defaults to EffortMedium when
	// empty.
	Effort Effort
}

// callOptions translates the Thinking config into fantasy provider options for
// the given provider, plus a minimum MaxOutputTokens the request should carry
// (0 means "leave the provider default"). It returns nil options when thinking
// cannot be expressed for the provider.
func (t *Thinking) callOptions(provider Provider) (fantasy.ProviderOptions, int64) {
	effort := t.Effort
	if effort == "" {
		effort = EffortMedium
	}

	switch provider {
	case "", ProviderAnthropic:
		// output_config.effort drives adaptive thinking; fantasy also opts into
		// summarized thinking display so the reasoning text is non-empty on
		// models that otherwise omit it.
		anthropicEffort := anthropic.Effort(effort)
		opts := fantasy.ProviderOptions{
			anthropic.Name: &anthropic.ProviderOptions{Effort: &anthropicEffort},
		}
		return opts, thinkingMaxOutputTokens
	case ProviderGoogle:
		// Gemini 3+ selects thinking depth by level (uppercase), and thoughts
		// must be requested to be returned.
		level := google.ThinkingLevel(strings.ToUpper(string(effort)))
		include := true
		opts := fantasy.ProviderOptions{
			google.Name: &google.ProviderOptions{
				ThinkingConfig: &google.ThinkingConfig{
					ThinkingLevel:   &level,
					IncludeThoughts: &include,
				},
			},
		}
		return opts, thinkingMaxOutputTokens
	case ProviderOpenAI:
		reasoning := openai.ReasoningEffort(effort)
		opts := fantasy.ProviderOptions{
			openai.Name: &openai.ProviderOptions{ReasoningEffort: &reasoning},
		}
		return opts, thinkingMaxOutputTokens
	default:
		return nil, 0
	}
}
