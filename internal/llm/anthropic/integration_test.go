//go:build integration

package anthropic

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests call the real Messages API with ANTHROPIC_API_KEY and are
// skipped without it. They cost a fraction of a cent. Run them through
// `make integration` with the key exported, for example:
//
//	ANTHROPIC_API_KEY=... make integration

func realClient(t *testing.T, thinking bool) *Client {
	t.Helper()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}
	prompt, err := os.ReadFile("../../../prompts/example.system.txt")
	if err != nil {
		t.Fatalf("read the example prompt: %v", err)
	}
	model := os.Getenv("PORTUNUS_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	c, err := New(Options{APIKey: key, Model: model, MaxTokens: 1024, SystemPrompt: string(prompt), Thinking: thinking})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestRealConversation takes a short message and checks that the model asks
// for what is missing, in Italian, and hangs up once it has it.
func TestRealConversation(t *testing.T) {
	c := realClient(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	conv := c.NewConversation(time.Now(), "Buongiorno, sono l'assistente automatico dell'ufficio. Come posso aiutarla?")
	r, err := conv.Reply(ctx, "Buongiorno, sono Mario Rossi, vorrei lasciare un messaggio per il geometra.", func(string) {})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	t.Logf("reply 1: %q (input %d, output %d, cache read %d, cache write %d)",
		r.Text, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens)
	if r.Text == "" || r.EndCall {
		t.Errorf("the model must ask for the missing details, not hang up: %+v", r)
	}

	r, err = conv.Reply(ctx, "Il mio numero è tre tre tre, uno due tre, quattro cinque sei sette. "+
		"Lo chiamo per il preventivo della ristrutturazione. È tutto, grazie, arrivederci.", func(string) {})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	t.Logf("reply 2: %q end_call=%v (cache read %d)", r.Text, r.EndCall, r.CacheReadTokens)
	if !r.EndCall {
		t.Error("with the message complete and the caller saying goodbye, the model must call end_call")
	}
	if strings.ContainsAny(r.Text, "*#") {
		t.Errorf("markdown in a spoken reply: %q", r.Text)
	}
}

// TestFirstTokenLatency measures how long the first word of a reply takes,
// with thinking off and on. It reports; it does not judge. The numbers decide
// PORTUNUS_LLM_THINKING.
func TestFirstTokenLatency(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		c := realClient(t, thinking)
		var samples []time.Duration
		for range 3 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			conv := c.NewConversation(time.Now(), "Buongiorno, come posso aiutarla?")
			start := time.Now()
			var first time.Duration
			_, err := conv.Reply(ctx, "Buongiorno, vorrei parlare con l'amministrazione.", func(string) {
				if first == 0 {
					first = time.Since(start)
				}
			})
			cancel()
			if err != nil {
				t.Fatalf("thinking=%v: %v", thinking, err)
			}
			samples = append(samples, first)
		}
		t.Logf("thinking=%v: first text after %v", thinking, samples)
	}
}
