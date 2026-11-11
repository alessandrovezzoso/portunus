//go:build integration

package openaicompat

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// TestRealServer synthesizes one Italian sentence on the server named by
// PORTUNUS_TTS_URL (default: the local Kokoro on 127.0.0.1:8880). Run it
// through `make integration`, with `make tts-up` first.
func TestRealServer(t *testing.T) {
	base := os.Getenv("PORTUNUS_TTS_URL")
	if base == "" {
		base = "http://127.0.0.1:8880"
	}
	c, err := New(Options{BaseURL: base, Model: "kokoro", Voice: "if_sara"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check: %v (is the server up? make tts-up)", err)
	}

	start := time.Now()
	body, err := c.Synthesize(ctx, "Buongiorno, questa è una prova della voce di Portunus.")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer func() { _ = body.Close() }()

	first := make([]byte, 1)
	if _, err := io.ReadFull(body, first); err != nil {
		t.Fatalf("no audio: %v", err)
	}
	firstAudio := time.Since(start)
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	seconds := float64(len(rest)+1) / 2 / SampleRate
	t.Logf("first audio after %v; %.2f s of speech in %v", firstAudio.Round(time.Millisecond), seconds, time.Since(start).Round(time.Millisecond))
	if seconds < 1 {
		t.Errorf("only %.2f s of audio for a full sentence", seconds)
	}
}
