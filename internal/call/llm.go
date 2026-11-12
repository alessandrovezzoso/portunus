package call

import (
	"context"
	"errors"
	"time"
)

// LLM is the language model that holds the conversation. It only makes
// conversations: each call gets its own, which keeps the history in memory
// and is thrown away when the call ends. Nothing is remembered across calls.
type LLM interface {
	NewConversation(info CallInfo) Conversation
}

// CallInfo is what the model may know about the call. It deliberately leaves
// out the caller's number: that is a third party's data, and the model asks
// for a callback number anyway.
type CallInfo struct {
	// Start is when the call was answered, for the date and time the model
	// sees ("call me back tomorrow" needs to know what today is).
	Start time.Time
	// Greeting is what Portunus already said when it answered, as the caller
	// heard it: plain words, without the TTS's pause tags.
	Greeting string
}

// Conversation is one call's exchange with the model.
type Conversation interface {
	// Reply sends what the caller said and streams the answer: onText gets
	// each piece of text as it arrives, so speech can start before the reply
	// is complete. Cancelling ctx must abort the stream.
	//
	// When Reply fails, the conversation is left as it was before the call,
	// and the returned Reply carries only Usage: the tokens already billed
	// for the failed attempt, if any.
	Reply(ctx context.Context, callerText string, onText func(string)) (Reply, error)
}

// Reply is a finished answer.
type Reply struct {
	Text string
	// EndCall reports that the model wants to hang up once Text is spoken.
	// This is how a turn ends with an action instead of only speech; call
	// transfer (v2) will be another such action.
	EndCall bool
	// Refused reports that the model's safety systems declined to answer.
	Refused bool
	Usage   Usage
}

// Usage counts the tokens of one reply, so the cost of a call can be read
// from its log.
type Usage struct {
	Input      int64 // uncached input tokens
	Output     int64 // output tokens, thinking included
	CacheRead  int64
	CacheWrite int64
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

// An adapter reports a failure that no retry within this call can fix (the
// credit is exhausted, the key was revoked) with an error that has an
// Unavailable method returning true. Portunus then stops answering, so the
// PBX sends callers to voicemail instead of to a mute assistant.
//
// It is a method, not a sentinel error here, because adapters never import
// this package.
func unavailable(err error) bool {
	var u interface{ Unavailable() bool }
	return errors.As(err, &u) && u.Unavailable()
}
