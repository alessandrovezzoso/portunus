package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests talk to an httptest server on the loopback interface that
// speaks the Messages API's server-sent events. No real API is involved.

// sse writes one streamed response: a text reply, optionally followed by a
// call to end_call.
func sse(w http.ResponseWriter, pieces []string, endCall bool, stop string) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"content": []any{}, "stop_reason": nil,
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 1,
			"cache_read_input_tokens": 900, "cache_creation_input_tokens": 30},
	}})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	for _, p := range pieces {
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": p}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	if endCall {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "end_call", "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	}
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 25}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

func newTestClient(t *testing.T, srv *httptest.Server, thinking bool) *Client {
	t.Helper()
	c, err := New(Options{
		APIKey: "test-key", Model: "claude-haiku-5-5", MaxTokens: 1024,
		SystemPrompt: "Sei un assistente telefonico.", Thinking: thinking,
		Location: time.UTC, BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestReplyStreamsAndSendsTheRightRequest(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This SDK version sends the key as a bearer token.
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request %s with authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			sse(w, []string{"Certo, ", "mi dica ", "il suo nome."}, false, "end_turn")
		} else {
			sse(w, []string{"Grazie, arrivederci."}, true, "tool_use")
		}
	}))
	defer srv.Close()

	start := time.Date(2026, 11, 12, 10, 5, 0, 0, time.UTC)
	conv := newTestClient(t, srv, false).NewConversation(start, "Buongiorno, sono l'assistente.")

	var streamed []string
	r, err := conv.Reply(context.Background(), "Vorrei lasciare un messaggio.", func(s string) { streamed = append(streamed, s) })
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if strings.Join(streamed, "") != "Certo, mi dica il suo nome." || len(streamed) != 3 {
		t.Errorf("streamed %q, want the three pieces as they came", streamed)
	}
	if r.Text != "Certo, mi dica il suo nome." || r.EndCall || r.Refused {
		t.Errorf("reply = %+v", r)
	}
	if r.InputTokens != 12 || r.OutputTokens != 25 || r.CacheReadTokens != 900 || r.CacheWriteTokens != 30 {
		t.Errorf("usage = %+v", r)
	}

	r, err = conv.Reply(context.Background(), "Sono Mario Rossi, grazie.", func(string) {})
	if err != nil || !r.EndCall {
		t.Fatalf("second reply: %+v, %v; want end_call", r, err)
	}

	first := bodies[0]
	if first["model"] != "claude-haiku-5-5" || first["max_tokens"] != float64(1024) {
		t.Errorf("model/max_tokens = %v/%v", first["model"], first["max_tokens"])
	}
	if _, ok := first["temperature"]; ok {
		t.Error("temperature must not be sent: current models reject it")
	}
	if eff := first["output_config"].(map[string]any)["effort"]; eff != "low" {
		t.Errorf("effort = %v, want low", eff)
	}
	if th := first["thinking"].(map[string]any)["type"]; th != "disabled" {
		t.Errorf("thinking = %v, want disabled", th)
	}
	if first["cache_control"] == nil {
		t.Error("no top-level cache_control: the conversation would not be cached")
	}
	system := first["system"].([]any)
	fixed, perCall := system[0].(map[string]any), system[1].(map[string]any)
	if fixed["cache_control"] == nil || perCall["cache_control"] != nil {
		t.Error("the breakpoint must sit on the fixed prompt, and the per-call block after it")
	}
	if !strings.Contains(fixed["text"].(string), "Do not include internal or system XML tags") {
		t.Error("with thinking off, the tool-discipline line must be in the prompt")
	}
	if ctx := perCall["text"].(string); !strings.Contains(ctx, "giovedì 12 novembre 2026") || !strings.Contains(ctx, "10:05") {
		t.Errorf("per-call context = %q", ctx)
	}
	tools := first["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "end_call" {
		t.Errorf("tools = %v, want only end_call", tools)
	}

	// The second request carries the whole history: user, assistant, user.
	msgs := bodies[1]["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("second request has %d messages, want the history of 3", len(msgs))
	}
}

func TestThinkingCanBeTurnedOn(t *testing.T) {
	var thinking string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		thinking, _ = body["thinking"].(map[string]any)["type"].(string)
		sse(w, []string{"Va bene."}, false, "end_turn")
	}))
	defer srv.Close()
	if _, err := newTestClient(t, srv, true).NewConversation(time.Now(), "Ciao").Reply(context.Background(), "x", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if thinking != "adaptive" {
		t.Errorf("thinking = %q, want adaptive", thinking)
	}
}

func TestReplyReportsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, nil, false, "refusal")
	}))
	defer srv.Close()
	r, err := newTestClient(t, srv, false).NewConversation(time.Now(), "Ciao").Reply(context.Background(), "x", func(string) {})
	if err != nil || !r.Refused {
		t.Errorf("reply %+v, %v; want a refusal", r, err)
	}
}

func apiError(status int, typ, msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg))
	}
}

func TestErrorsAreClassified(t *testing.T) {
	for _, tt := range []struct {
		name        string
		handler     http.HandlerFunc
		unavailable bool
		fatal       bool
	}{
		{"no credit", apiError(402, "billing_error", "Payment required"), true, false},
		{"low balance", apiError(400, "invalid_request_error", "Your credit balance is too low to access the API."), true, false},
		{"bad key", apiError(401, "authentication_error", "invalid x-api-key"), true, true},
		{"no permission", apiError(403, "permission_error", "not allowed"), true, true},
		{"overloaded", apiError(529, "overloaded_error", "Overloaded"), false, false},
		{"bad model", apiError(404, "not_found_error", "model: claude-nope"), false, true},
	} {
		srv := httptest.NewServer(tt.handler)
		c := newTestClient(t, srv, false)
		_, err := c.NewConversation(time.Now(), "Ciao").Reply(context.Background(), "x", func(string) {})
		checkErr := c.Check(context.Background())
		srv.Close()

		var u interface{ Unavailable() bool }
		if err == nil || !errorsAs(err, &u) || u.Unavailable() != tt.unavailable {
			t.Errorf("%s: Reply error %v, want unavailable=%v", tt.name, err, tt.unavailable)
		}
		if Fatal(checkErr) != tt.fatal {
			t.Errorf("%s: Check error %v, want fatal=%v", tt.name, checkErr, tt.fatal)
		}
	}
}

func TestFailedReplyLeavesTheHistoryAlone(t *testing.T) {
	fail := true
	var lastLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastLen = len(body["messages"].([]any))
		if fail {
			apiError(500, "api_error", "boom")(w, r)
			return
		}
		sse(w, []string{"Va bene."}, false, "end_turn")
	}))
	defer srv.Close()
	conv := newTestClient(t, srv, false).NewConversation(time.Now(), "Ciao")
	if _, err := conv.Reply(context.Background(), "prima", func(string) {}); err == nil {
		t.Fatal("expected an error")
	}
	fail = false
	if _, err := conv.Reply(context.Background(), "seconda", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if lastLen != 1 {
		t.Errorf("after a failed reply the next request has %d messages, want 1", lastLen)
	}
}

func TestCheckUsesTheModelsEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models/claude-haiku-5-5" {
			t.Errorf("Check called %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"model","id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","created_at":"2026-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	if err := newTestClient(t, srv, false).Check(context.Background()); err != nil {
		t.Errorf("Check on a healthy server: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	ok := Options{APIKey: "k", Model: "m", MaxTokens: 10, SystemPrompt: "p"}
	for _, broken := range []func(*Options){
		func(o *Options) { o.APIKey = "" },
		func(o *Options) { o.Model = "" },
		func(o *Options) { o.MaxTokens = 0 },
		func(o *Options) { o.SystemPrompt = "  " },
	} {
		o := ok
		broken(&o)
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v): expected an error", o)
		}
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func TestProbe(t *testing.T) {
	var maxTokens float64
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		maxTokens, _ = body["max_tokens"].(float64)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5",`+
			`"content":[{"type":"text","text":"p"}],"stop_reason":"max_tokens","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer ok.Close()
	if err := newTestClient(t, ok, false).Probe(context.Background()); err != nil || maxTokens != 1 {
		t.Errorf("Probe = %v with max_tokens %v, want success with 1", err, maxTokens)
	}

	broke := httptest.NewServer(apiError(402, "billing_error", "Payment required"))
	defer broke.Close()
	err := newTestClient(t, broke, false).Probe(context.Background())
	var u interface{ Unavailable() bool }
	if err == nil || !errorsAs(err, &u) || !u.Unavailable() {
		t.Errorf("Probe with no credit = %v, want an unavailable error", err)
	}
}

// TestEnvironmentIsIgnored sets the variables the SDK would read on its own.
// Portunus's configuration must be the only one in effect: no extra header,
// and no other server, may come from the process environment.
func TestEnvironmentIsIgnored(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: yes")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "someone-elses-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Leak") != "" {
			t.Error("a header from ANTHROPIC_CUSTOM_HEADERS reached the server")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization %q, want the configured key only", got)
		}
		sse(w, []string{"Buongiorno, mi dica pure."}, false, "end_turn")
	}))
	defer srv.Close()

	c := newTestClient(t, srv, false)
	if _, err := c.NewConversation(time.Now(), "Buongiorno.").Reply(context.Background(), "Pronto?", func(string) {}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
}

// TestUsageSurvivesAFailedStream cuts a stream after message_start: the input
// was billed, so the failed Reply must still report it.
func TestUsageSurvivesAFailedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: "+
			`{"type":"message_start","message":{"id":"msg_cut","type":"message","role":"assistant",`+
			`"model":"claude-haiku-5-5","content":[],"stop_reason":null,`+
			`"usage":{"input_tokens":40,"output_tokens":1,"cache_read_input_tokens":800,"cache_creation_input_tokens":0}}}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: error\ndata: "+
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, srv, false)
	r, err := c.NewConversation(time.Now(), "Buongiorno.").Reply(context.Background(), "Pronto?", func(string) {})
	if err == nil {
		t.Fatal("a stream that ends in an error must fail")
	}
	if r.InputTokens != 40 || r.CacheReadTokens != 800 {
		t.Errorf("usage of the failed reply = %+v, want the 40 input and 800 cache-read tokens billed", r)
	}
	if r.Text != "" || r.EndCall {
		t.Errorf("a failed reply must carry usage only, got %+v", r)
	}
}
