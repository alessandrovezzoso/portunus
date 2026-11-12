package main

import (
	"context"

	"github.com/alessandrovezzoso/portunus/internal/call"
	"github.com/alessandrovezzoso/portunus/internal/llm/anthropic"
)

// claudeLLM adapts the Anthropic adapter to call.LLM. Adapters never import
// internal/call, so the adapter has its own types, and this is the one place
// that knows both.
type claudeLLM struct{ c *anthropic.Client }

func (l claudeLLM) NewConversation(info call.CallInfo) call.Conversation {
	return claudeConversation{l.c.NewConversation(info.Start, info.Greeting)}
}

type claudeConversation struct{ c *anthropic.Conversation }

// Reply passes the usage on even when the reply fails: those tokens were
// billed, and the end-of-call log must count them.
func (v claudeConversation) Reply(ctx context.Context, callerText string, onText func(string)) (call.Reply, error) {
	r, err := v.c.Reply(ctx, callerText, onText)
	usage := call.Usage{
		Input:      r.InputTokens,
		Output:     r.OutputTokens,
		CacheRead:  r.CacheReadTokens,
		CacheWrite: r.CacheWriteTokens,
	}
	if err != nil {
		return call.Reply{Usage: usage}, err
	}
	return call.Reply{Text: r.Text, EndCall: r.EndCall, Refused: r.Refused, Usage: usage}, nil
}
