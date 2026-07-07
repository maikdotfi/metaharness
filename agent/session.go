package agent

import "charm.land/fantasy"

type Status string

const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Session is the per-conversation state. One per goroutine; the thing the store persists.
type Session struct {
	ID       string
	Model    string            // reassign to switch model within the provider
	Messages []fantasy.Message // the transcript IS fantasy's type
	Usage    fantasy.Usage
	Status   Status
	Sandbox  SandboxSpec
}

func addUsage(dst *fantasy.Usage, u fantasy.Usage) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.TotalTokens += u.TotalTokens
	dst.ReasoningTokens += u.ReasoningTokens
	dst.CacheCreationTokens += u.CacheCreationTokens
	dst.CacheReadTokens += u.CacheReadTokens
}
