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

	"github.com/alessandrovezzoso/portunus/internal/llm/anthropic"
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

// TestAgentOnRealEngines runs one turn of the agent mode on every real
// engine: Kokoro says the caller's phrase, which goes in as µ-law RTP; the
// real Silero, Whisper and Claude handle it; Kokoro speaks the reply. It
// needs make tts-up, make stt-up and ANTHROPIC_API_KEY, and is skipped
// without the key.
func TestAgentOnRealEngines(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prompt, err := os.ReadFile("../../prompts/example.system.txt")
	if err != nil {
		t.Fatal(err)
	}
	llm, err := anthropic.New(anthropic.Options{APIKey: key, Model: "claude-haiku-5-5", MaxTokens: 1024, SystemPrompt: string(prompt)})
	if err != nil {
		t.Fatal(err)
	}
	tts, err := ttsapi.New(ttsapi.Options{BaseURL: envOr("PORTUNUS_TTS_URL", "http://127.0.0.1:8880"), Model: "kokoro", Voice: "if_sara"})
	if err != nil {
		t.Fatal(err)
	}
	stt, err := sttapi.New(sttapi.Options{BaseURL: envOr("PORTUNUS_STT_URL", "http://127.0.0.1:8890"), Model: "large-v3-turbo", Language: "it"})
	if err != nil {
		t.Fatal(err)
	}
	holding, err := PrepareHolding(ctx, tts, DefaultHoldingPhrases)
	if err != nil {
		t.Fatalf("holding phrases: %v (is Kokoro up? make tts-up)", err)
	}

	// The greeting plays first; the caller speaks once it is over, in fast
	// motion (one frame per millisecond), then stays silent long enough, in
	// real time, for the answer.
	caller := phoneFrames(ctx, t, "Buongiorno, sono Mario Rossi. Vorrei lasciare un messaggio per il geometra.")
	in := &pacedInbound{frames: script(silenceFrames(3000), caller, silenceFrames(12000)), hangup: make(chan struct{})}

	var logs bytes.Buffer
	h := Agent(testLLM{llm}, stt, tts, func() (VAD, error) { return silero.New() }, AgentOptions{
		Greeting:     "Buongiorno, sono l'assistente automatico dell'ufficio. Come posso aiutarla?",
		Holding:      holding,
		OutputBuffer: 20 * time.Second,
		Logger:       slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		tail:         5 * time.Millisecond,
	})
	go func() {
		<-ctx.Done()
		closeOnce(in.hangup)
	}()
	if err := h(ctx, &sip.Call{ID: "agent-integration", Codec: sip.CodecPCMU, Inbound: in, Outbound: &pacedSink{}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Logf("\n%s", logs.String())
	if !strings.Contains(logs.String(), `msg="turn latency"`) {
		t.Error("no turn was answered")
	}
}

// testLLM adapts the Anthropic adapter to LLM, as cmd/portunus does.
type testLLM struct{ c *anthropic.Client }

func (l testLLM) NewConversation(info CallInfo) Conversation {
	return testConv{l.c.NewConversation(info.Start, info.Greeting)}
}

type testConv struct{ c *anthropic.Conversation }

func (v testConv) Reply(ctx context.Context, text string, onText func(string)) (Reply, error) {
	r, err := v.c.Reply(ctx, text, onText)
	return Reply{Text: r.Text, EndCall: r.EndCall, Refused: r.Refused,
		Usage: Usage{Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens, CacheWrite: r.CacheWriteTokens}}, err
}
