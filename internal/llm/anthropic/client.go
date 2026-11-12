// Package anthropic is the LLM adapter for Claude, through Anthropic's
// official Go SDK. It keeps one conversation per call, streams replies so
// speech can start early, caches the fixed system prompt, and gives the model
// one tool: end_call.
//
// It knows nothing about calls or telephony beyond the start time and the
// greeting, and it never imports internal/call: cmd/portunus adapts its types.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Options configures a Client.
type Options struct {
	APIKey string
	// Model comes from config (PORTUNUS_LLM_MODEL) and is never hardcoded.
	Model string
	// MaxTokens caps each reply, thinking included. Phone replies are short,
	// so this is a cost ceiling more than a length limit.
	MaxTokens int64
	// SystemPrompt is the fixed prompt. It is the same for every call, so it
	// is sent with prompt caching.
	SystemPrompt string
	// Thinking lets the model reason before it answers. It costs time before
	// the first word, which on the phone is silence.
	Thinking bool
	// Location is the time zone of the date and time the model sees. Nil
	// means the local zone.
	Location *time.Location

	// For tests: a local server, and the number of retries (zero means the
	// SDK's default of 2, negative means none).
	BaseURL    string
	HTTPClient *http.Client
	MaxRetries int
}

// Client talks to the Messages API.
type Client struct {
	api    sdk.Client
	opts   Options
	system sdk.TextBlockParam
}

// requestTimeout bounds one reply. A phone reply is a few sentences: a
// request still running after this is stuck.
const requestTimeout = 30 * time.Second

// New returns a client for opts. It does not contact the API.
func New(opts Options) (*Client, error) {
	switch {
	case opts.APIKey == "":
		return nil, errors.New("llm: the API key is required")
	case opts.Model == "":
		return nil, errors.New("llm: the model is required")
	case opts.MaxTokens <= 0:
		return nil, errors.New("llm: max tokens must be positive")
	case strings.TrimSpace(opts.SystemPrompt) == "":
		return nil, errors.New("llm: the system prompt is empty")
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}

	ro := []option.RequestOption{
		option.WithAPIKey(opts.APIKey),
		option.WithRequestTimeout(requestTimeout),
	}
	switch {
	case opts.MaxRetries < 0:
		ro = append(ro, option.WithMaxRetries(0))
	case opts.MaxRetries > 0:
		ro = append(ro, option.WithMaxRetries(opts.MaxRetries))
	}
	if opts.BaseURL != "" {
		ro = append(ro, option.WithBaseURL(opts.BaseURL))
	}
	if opts.HTTPClient != nil {
		ro = append(ro, option.WithHTTPClient(opts.HTTPClient))
	}

	prompt := opts.SystemPrompt
	if !opts.Thinking {
		// With thinking off, the model can write a tool call into its text
		// instead of calling the tool. This is the countermeasure Anthropic's
		// guidance gives for it.
		prompt += "\n\n" + toolDiscipline
	}
	return &Client{
		api:  sdk.NewClient(ro...),
		opts: opts,
		system: sdk.TextBlockParam{
			Text:         prompt,
			CacheControl: sdk.NewCacheControlEphemeralParam(),
		},
	}, nil
}

const toolDiscipline = "When you use a tool, you may say a brief sentence first. " +
	"If no tool can express what the user asked for, say so instead of guessing. " +
	"Do not include internal or system XML tags in your response."

// Check verifies the key and the model without spending tokens. A rejected
// key or an unknown model is reported with an error for which Fatal is true:
// Portunus must not register then, so the PBX sends calls to its fallback.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.api.Models.Get(ctx, c.opts.Model, sdk.ModelGetParams{})
	if err == nil {
		return nil
	}
	return classify(err)
}

// Probe sends the smallest real request, one token long, to learn whether the
// account can be billed again. Check cannot tell: listing a model costs
// nothing, so it works with no credit at all. Probe costs a few tokens, so it
// runs only while the model is known to be unavailable, and rarely.
func (c *Client) Probe(ctx context.Context) error {
	_, err := c.api.Messages.New(ctx, sdk.MessageNewParams{
		Model:     c.opts.Model,
		MaxTokens: 1,
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock("ping"))},
		Thinking:  sdk.ThinkingConfigParamUnion{OfDisabled: &sdk.ThinkingConfigDisabledParam{}},
	})
	if err != nil {
		return classify(err)
	}
	return nil
}

// endCall is the model's only tool. It has no input: it hangs up, once the
// text of the same reply has been spoken, and does nothing else.
const endCall = "end_call"

var endCallTool = sdk.ToolParam{
	Name: endCall,
	Description: sdk.String("Hang up the phone call. Call it only in a reply that also says goodbye " +
		"to the caller, once the message is complete or the caller wants to end the call. " +
		"The goodbye is spoken first, then the call ends."),
	InputSchema: sdk.ToolInputSchemaParam{Properties: map[string]any{}},
}

// Conversation is one call's history with the model.
type Conversation struct {
	c       *Client
	context sdk.TextBlockParam
	history []sdk.MessageParam
}

// NewConversation starts the history of one call.
func (c *Client) NewConversation(start time.Time, greeting string) *Conversation {
	return &Conversation{c: c, context: sdk.TextBlockParam{Text: callContext(start.In(c.opts.Location), greeting)}}
}

// Reply is a finished answer, in this package's own types.
type Reply struct {
	Text    string
	EndCall bool
	Refused bool

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Reply sends what the caller said and streams the answer to onText as it
// arrives. On failure the history is left as it was.
func (v *Conversation) Reply(ctx context.Context, callerText string, onText func(string)) (Reply, error) {
	messages := append(v.history, sdk.NewUserMessage(sdk.NewTextBlock(callerText)))

	thinking := sdk.ThinkingConfigParamUnion{OfDisabled: &sdk.ThinkingConfigDisabledParam{}}
	if v.c.opts.Thinking {
		thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{}}
	}
	params := sdk.MessageNewParams{
		Model:     v.c.opts.Model,
		MaxTokens: v.c.opts.MaxTokens,
		// The fixed prompt carries the cache breakpoint. The call's own
		// context comes after it, so it never invalidates the cached prefix.
		System:   []sdk.TextBlockParam{v.c.system, v.context},
		Messages: messages,
		Tools:    []sdk.ToolUnionParam{{OfTool: &endCallTool}},
		Thinking: thinking,
		// Low effort: on the phone, latency beats eloquence.
		OutputConfig: sdk.OutputConfigParam{Effort: sdk.OutputConfigEffortLow},
		// A second breakpoint, placed by the API on the last block, caches
		// the conversation so far: each turn pays full price only for what
		// is new.
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}

	stream := v.c.api.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg sdk.Message
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return Reply{}, fmt.Errorf("llm: %w", err)
		}
		if delta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent); ok {
			if t, ok := delta.Delta.AsAny().(sdk.TextDelta); ok && t.Text != "" {
				onText(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Reply{}, classify(err)
	}

	r := Reply{
		Refused:          msg.StopReason == sdk.StopReasonRefusal,
		InputTokens:      msg.Usage.InputTokens,
		OutputTokens:     msg.Usage.OutputTokens,
		CacheReadTokens:  msg.Usage.CacheReadInputTokens,
		CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
	}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			text.WriteString(b.Text)
		case sdk.ToolUseBlock:
			r.EndCall = r.EndCall || b.Name == endCall
		}
	}
	r.Text = text.String()

	// The whole response goes back into the history, thinking blocks
	// included and unmodified, as the API requires.
	v.history = append(messages, msg.ToParam())
	return r, nil
}

// Error is a failed request. Unavailable reports a failure no retry within a
// call can fix: the credit is exhausted, or the key is rejected or lacks
// permission. Fatal reports one that must stop Portunus at startup.
type Error struct {
	err         error
	unavailable bool
	fatal       bool
}

func (e *Error) Error() string     { return "llm: " + e.err.Error() }
func (e *Error) Unwrap() error     { return e.err }
func (e *Error) Unavailable() bool { return e.unavailable }

// Fatal reports whether err from Check means Portunus must not start: a
// rejected key, or a model that does not exist.
func Fatal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.fatal
}

func classify(err error) error {
	var api *sdk.Error
	if !errors.As(err, &api) {
		return &Error{err: err} // the network: worth a warning, not a stop
	}
	switch {
	case api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden:
		return &Error{err: err, unavailable: true, fatal: true}
	case api.StatusCode == http.StatusPaymentRequired || api.Type() == "billing_error":
		return &Error{err: err, unavailable: true}
	case api.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(api.Message), "credit balance"):
		// An exhausted balance has also been reported as a 400. The SDK
		// warns that the wording can change; this is a best effort on top
		// of the 402 above.
		return &Error{err: err, unavailable: true}
	case api.StatusCode == http.StatusNotFound:
		return &Error{err: err, fatal: true}
	}
	return &Error{err: err}
}

var (
	weekdays = [...]string{"domenica", "lunedì", "martedì", "mercoledì", "giovedì", "venerdì", "sabato"}
	months   = [...]string{"", "gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno",
		"luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"}
)

// callContext is the part of the system prompt that changes with each call.
// It sits after the cache breakpoint.
func callContext(t time.Time, greeting string) string {
	return fmt.Sprintf("Contesto di questa chiamata, da non leggere a voce: oggi è %s %d %s %d e sono le %02d:%02d. "+
		"All'inizio della chiamata hai già detto: «%s»",
		weekdays[t.Weekday()], t.Day(), months[t.Month()], t.Year(), t.Hour(), t.Minute(), greeting)
}
