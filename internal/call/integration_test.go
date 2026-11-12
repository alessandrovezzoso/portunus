//go:build integration

package call

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
	sttapi "github.com/alessandrovezzoso/portunus/internal/stt/openaicompat"
	ttsapi "github.com/alessandrovezzoso/portunus/internal/tts/openaicompat"
	"github.com/alessandrovezzoso/portunus/internal/vad/silero"
)

// TestListenOnRealEngines runs the listen mode the way a phone call does,
// minus the network: Kokoro says a sentence, the speech is encoded to µ-law
// at 8 kHz in 20 ms RTP payloads, and Listen hears it through the real Silero
// VAD and transcribes it with the real Whisper. It needs make tts-up and make
// stt-up.
func TestListenOnRealEngines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	phrase := "Buongiorno, sono Mario Rossi. Vorrei essere richiamato domani mattina."
	frames := script(silenceFrames(25), phoneFrames(ctx, t, phrase), silenceFrames(75))

	stt, err := sttapi.New(sttapi.Options{BaseURL: envOr("PORTUNUS_STT_URL", "http://127.0.0.1:8890"), Model: "large-v3-turbo", Language: "it"})
	if err != nil {
		t.Fatalf("stt: %v", err)
	}
	var logs bytes.Buffer
	h := Listen(stt, func() (VAD, error) { return silero.New() }, ListenOptions{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	start := time.Now()
	err = h(ctx, &sip.Call{ID: "integration", Codec: sip.CodecPCMU, Inbound: &scriptedInbound{frames: frames}, Outbound: &pacedSink{}})
	if err != nil {
		t.Fatalf("handler: %v\n%s", err, logs.String())
	}
	t.Logf("handled in %v\n%s", time.Since(start).Round(time.Millisecond), logs.String())

	out := strings.ToLower(logs.String())
	if strings.Count(out, "msg=utterance") < 1 {
		t.Fatal("Silero heard no utterance in Kokoro's speech")
	}
	for _, word := range []string{"buongiorno", "mario", "rossi", "richiamato"} {
		if !strings.Contains(out, word) {
			t.Errorf("transcript lacks %q", word)
		}
	}
}

// phoneFrames has Kokoro say text and returns it as the S20 would send it: µ-law
// at 8 kHz, 160 bytes per RTP payload.
func phoneFrames(ctx context.Context, t *testing.T, text string) [][]byte {
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

	enc := media.NewEncoder(ttsapi.SampleRate, media.ULaw)
	r := media.NewPCM16Reader(body)
	buf := make([]int16, 4800)
	var g711 []byte
	for {
		n, err := r.Read(buf)
		g711 = append(g711, enc.Encode(buf[:n])...)
		if err != nil {
			break
		}
	}
	g711 = append(g711, enc.Flush()...)

	var frames [][]byte
	for len(g711) >= media.FrameSize {
		frames = append(frames, g711[:media.FrameSize])
		g711 = g711[media.FrameSize:]
	}
	return frames
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
