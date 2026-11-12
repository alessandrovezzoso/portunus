//go:build integration

package openaicompat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	ttsapi "github.com/alessandrovezzoso/portunus/internal/tts/openaicompat"
)

// TestRealServer has Kokoro say an Italian sentence and Whisper transcribe it,
// on the servers named by PORTUNUS_TTS_URL and PORTUNUS_STT_URL (default: the
// local ones). Run it through `make integration`, after make tts-up and make
// stt-up.
func TestRealServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	speech := synthesize(ctx, t, "Buongiorno, vorrei parlare con l'ufficio tecnico, grazie.")

	c, err := New(Options{BaseURL: envOr("PORTUNUS_STT_URL", "http://127.0.0.1:8890"), Model: "large-v3-turbo", Language: "it"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check: %v (is the server up? make stt-up)", err)
	}
	t.Logf("check (one second of silence, warms the model up): %v", time.Since(start).Round(time.Millisecond))

	start = time.Now()
	text, err := c.Transcribe(ctx, speech)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("%.1f s of speech transcribed in %v: %q",
		float64(len(speech))/SampleRate, time.Since(start).Round(time.Millisecond), text)
	for _, word := range []string{"buongiorno", "ufficio", "tecnico"} {
		if !strings.Contains(strings.ToLower(text), word) {
			t.Errorf("transcript lacks %q", word)
		}
	}
}

// synthesize returns Kokoro's speech for text as PCM at SampleRate.
func synthesize(ctx context.Context, t *testing.T, text string) []int16 {
	t.Helper()
	tts, err := ttsapi.New(ttsapi.Options{BaseURL: envOr("PORTUNUS_TTS_URL", "http://127.0.0.1:8880"), Model: "kokoro", Voice: "if_sara"})
	if err != nil {
		t.Fatalf("tts: %v", err)
	}
	body, err := tts.Synthesize(ctx, text)
	if err != nil {
		t.Fatalf("Synthesize: %v (is Kokoro up? make tts-up)", err)
	}
	defer func() { _ = body.Close() }()

	r := media.NewPCM16Reader(body)
	rs := media.NewResampler(ttsapi.SampleRate, SampleRate)
	buf := make([]int16, 4800)
	var out []int16
	for {
		n, err := r.Read(buf)
		out = rs.Process(out, buf[:n])
		if err != nil {
			break
		}
	}
	return rs.Flush(out)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
