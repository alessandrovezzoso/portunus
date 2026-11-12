package call

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// The agent tests run calls in fast motion: one inbound frame and one
// outbound frame per millisecond instead of every 20 ms. Durations of audio
// below are therefore 20 times shorter in wall time.

// pacedInbound plays its frames one per millisecond, like a caller in fast
// motion, then keeps the line open in silence until hangup is closed.
type pacedInbound struct {
	frames [][]byte
	next   int
	hangup chan struct{}
}

func (p *pacedInbound) Read(b []byte) (int, error) {
	if p.next < len(p.frames) {
		time.Sleep(time.Millisecond)
		n := copy(b, p.frames[p.next])
		p.next++
		return n, nil
	}
	select {
	case <-p.hangup:
		return 0, io.EOF
	case <-time.After(time.Millisecond):
		return copy(b, silenceFrames(1)[0]), nil
	}
}

// recordingTTS says everything as the same short tone, and keeps the texts.
type recordingTTS struct {
	audio time.Duration // of each utterance, at 8 kHz

	mu    sync.Mutex
	texts []string
}

func (r *recordingTTS) SampleRate() int { return media.TelephoneRate }

func (r *recordingTTS) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
	d := r.audio
	if d == 0 {
		d = 200 * time.Millisecond
	}
	return io.NopCloser(bytes.NewReader(toneBytes(media.TelephoneRate, d))), nil
}

func (r *recordingTTS) said() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

// scriptedLLM plays its replies in order, streaming each in small pieces.
type scriptedLLM struct {
	replies []fakeReply

	mu      sync.Mutex
	callers []string
	started chan struct{} // closed when the first reply starts, if set
}

type fakeReply struct {
	text    string
	endCall bool
	refused bool
	err     error
	block   bool // stream one piece, then wait for the context to end
}

func (s *scriptedLLM) NewConversation(CallInfo) Conversation { return &scriptedConv{llm: s} }

type scriptedConv struct {
	llm  *scriptedLLM
	turn int
}

func (c *scriptedConv) Reply(ctx context.Context, caller string, onText func(string)) (Reply, error) {
	c.llm.mu.Lock()
	c.llm.callers = append(c.llm.callers, caller)
	if c.llm.started != nil && c.turn == 0 {
		close(c.llm.started)
	}
	c.llm.mu.Unlock()

	if c.turn >= len(c.llm.replies) {
		return Reply{}, errors.New("script exhausted")
	}
	r := c.llm.replies[c.turn]
	c.turn++
	if r.err != nil {
		return Reply{}, r.err
	}
	for i := 0; i < len(r.text); i += 7 {
		onText(r.text[i:min(i+7, len(r.text))])
		if r.block {
			<-ctx.Done()
			return Reply{}, ctx.Err()
		}
	}
	return Reply{Text: r.text, EndCall: r.endCall, Refused: r.refused,
		Usage: Usage{Input: 100, Output: 20, CacheRead: 1000}}, nil
}

func (s *scriptedLLM) heard() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.callers...)
}

type unavailableErr struct{}

func (unavailableErr) Error() string     { return "credit balance too low" }
func (unavailableErr) Unavailable() bool { return true }

const testGreeting = "Buongiorno, risponde l'assistente automatico dell'ufficio."

// runAgent runs one call and returns its log. The caller hangs up when the
// handler returns, or earlier if the test closes in.hangup itself.
func runAgent(t *testing.T, llm LLM, stt STT, tts TTS, in *pacedInbound, opts AgentOptions) (string, *pacedSink) {
	t.Helper()
	var logs bytes.Buffer
	opts.Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts.Greeting = testGreeting
	opts.OutputBuffer = 20 * time.Second
	opts.VADSilence = 200 * time.Millisecond // ten frames
	opts.tail = 5 * time.Millisecond
	opts.muteTail = 5 * time.Millisecond
	out := &pacedSink{}

	h := Agent(llm, stt, tts, newLoudVAD, opts)
	done := make(chan error, 1)
	go func() {
		done <- h(context.Background(), &sip.Call{ID: "agent-test", Codec: sip.CodecPCMU, Inbound: in, Outbound: out})
	}()
	select {
	case err := <-done:
		closeOnce(in.hangup)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the call did not end\n%s", logs.String())
	}
	return logs.String(), out
}

// syncWriter serializes writes, so the log can be read safely at the end.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func closeOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

// phrase is one caller utterance followed by enough silence, in fast motion,
// for the assistant to answer before the next one starts.
func phrase() [][]byte { return script(toneFrames(20), silenceFrames(400)) }

func TestAgentHoldsAConversationAndHangsUp(t *testing.T) {
	llm := &scriptedLLM{replies: []fakeReply{
		{text: "Certo, mi dica pure il suo nome e il numero da richiamare."},
		{text: "Grazie, la faremo richiamare al più presto. Arrivederci.", endCall: true},
	}}
	stt := &fakeSTT{rate: media.TelephoneRate}
	tts := &recordingTTS{}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase(), phrase()), hangup: make(chan struct{})}

	logs, _ := runAgent(t, llm, stt, tts, in, AgentOptions{})

	if got := llm.heard(); strings.Join(got, "|") != "frase 1|frase 2" {
		t.Errorf("the model heard %q, want the two phrases", got)
	}
	said := strings.Join(tts.said(), " | ")
	for _, want := range []string{testGreeting, "mi dica pure il suo nome", "Arrivederci."} {
		if !strings.Contains(said, want) {
			t.Errorf("the assistant never said %q\nsaid: %s", want, said)
		}
	}
	for _, want := range []string{
		`msg="turn latency" call_id=agent-test turn=1 stt_ms=`,
		`msg="turn latency" call_id=agent-test turn=2 stt_ms=`,
		`first_audio_ms=`,
		`msg="call usage" call_id=agent-test input_tokens=200 output_tokens=40 cache_read_tokens=2000`,
		`msg="conversation ended" call_id=agent-test reason=assistant turns=2`,
		`caller="frase 1"`, // the texts, at debug level
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q\n%s", want, logs)
		}
	}
}

func TestAgentPlaysAHoldingPhrase(t *testing.T) {
	// A holding clip of a loud constant, whose µ-law byte is easy to find
	// among the tone frames.
	clip := make([]int16, 400)
	for i := range clip {
		clip[i] = 30000
	}
	marker := media.ULaw.Encode(nil, clip[:1])[0]
	llm := &scriptedLLM{replies: []fakeReply{{text: "Va bene, prendo nota e la saluto. Arrivederci.", endCall: true}}}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase()), hangup: make(chan struct{})}

	_, out := runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate}, &recordingTTS{}, in, AgentOptions{Holding: [][]int16{clip}})

	out.mu.Lock()
	defer out.mu.Unlock()
	found := false
	for _, f := range out.frames {
		if bytes.Count(f, []byte{marker}) == len(f) {
			found = true
		}
	}
	if !found {
		t.Error("the holding phrase never reached the wire")
	}
}

func TestAgentTurnCeiling(t *testing.T) {
	llm := &scriptedLLM{replies: []fakeReply{
		{text: "Certo, mi dica pure il suo nome e il numero da richiamare."},
		{text: "This reply must never be requested."},
	}}
	tts := &recordingTTS{}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase(), phrase()), hangup: make(chan struct{})}

	logs, _ := runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate}, tts, in, AgentOptions{MaxTurns: 1})

	if n := len(llm.heard()); n != 1 {
		t.Errorf("%d requests to the model, want 1: the ceiling must stop the second", n)
	}
	if said := tts.said(); said[len(said)-1] != lineClosing {
		t.Errorf("last words %q, want the closing line", said[len(said)-1])
	}
	if !strings.Contains(logs, "reason=max_turns") {
		t.Errorf("log lacks the reason\n%s", logs)
	}
}

func TestAgentCallTimeCeiling(t *testing.T) {
	tts := &recordingTTS{}
	in := &pacedInbound{hangup: make(chan struct{})} // a line that stays silent
	logs, _ := runAgent(t, &scriptedLLM{}, &fakeSTT{rate: media.TelephoneRate}, tts, in,
		AgentOptions{MaxCallTime: 150 * time.Millisecond})

	if said := tts.said(); len(said) != 2 || said[1] != lineClosing {
		t.Errorf("said %q, want the greeting and then the closing line", said)
	}
	if !strings.Contains(logs, "reason=max_call_time") {
		t.Errorf("log lacks the reason\n%s", logs)
	}
}

func TestAgentCallerHangsUpMidReply(t *testing.T) {
	started := make(chan struct{})
	llm := &scriptedLLM{started: started, replies: []fakeReply{{text: "Certo, mi dica pure con calma.", block: true}}}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase()), hangup: make(chan struct{})}
	go func() {
		<-started
		close(in.hangup)
	}()

	logs, _ := runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate}, &recordingTTS{}, in, AgentOptions{})
	if !strings.Contains(logs, "reason=caller") {
		t.Errorf("a hangup mid-reply must end the call as the caller's\n%s", logs)
	}
}

func TestAgentIgnoresSpeechWhileTalking(t *testing.T) {
	llm := &scriptedLLM{replies: []fakeReply{
		{text: "Certo, mi dica pure il suo nome e il numero da richiamare."},
		{text: "Grazie, la faremo richiamare al più presto. Arrivederci.", endCall: true},
	}}
	stt := &fakeSTT{rate: media.TelephoneRate}
	// The reply lasts 8 s of audio, 400 ms in fast motion. A second burst of
	// speech 60 ms after the first phrase lands right in the middle of it.
	tts := &recordingTTS{audio: 8 * time.Second}
	in := &pacedInbound{frames: script(
		silenceFrames(600), // the greeting is long too
		toneFrames(20), silenceFrames(60),
		toneFrames(20), silenceFrames(900), // talked over the reply
		phrase(),
	), hangup: make(chan struct{})}

	logs, _ := runAgent(t, llm, stt, tts, in, AgentOptions{})

	if n := len(stt.requests()); n != 2 {
		t.Errorf("%d transcriptions, want 2: speech over the reply must be ignored\n%s", n, logs)
	}
	if !strings.Contains(logs, "ignored_utterances=1") {
		t.Errorf("log must count the ignored speech\n%s", logs)
	}
}

func TestAgentAsksAgainWhenTheSTTFails(t *testing.T) {
	tts := &recordingTTS{}
	llm := &scriptedLLM{}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase()), hangup: make(chan struct{})}
	go func() {
		time.Sleep(time.Second)
		close(in.hangup)
	}()
	logs, _ := runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate, err: errors.New("connection refused")}, tts, in, AgentOptions{})

	if !strings.Contains(strings.Join(tts.said(), "|"), lineNotUnderstood) {
		t.Errorf("the assistant must ask the caller to repeat, said %q", tts.said())
	}
	if len(llm.heard()) != 0 {
		t.Error("nothing was understood, so nothing must reach the model")
	}
	if !strings.Contains(logs, "transcription failed") || !strings.Contains(logs, "reason=caller") {
		t.Errorf("log must show the failure and the hangup\n%s", logs)
	}
}

func TestAgentStopsWhenTheModelIsUnavailable(t *testing.T) {
	var told atomic.Bool
	tts := &recordingTTS{}
	llm := &scriptedLLM{replies: []fakeReply{{err: unavailableErr{}}}}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase()), hangup: make(chan struct{})}

	logs, _ := runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate}, tts, in,
		AgentOptions{OnUnavailable: func(error) { told.Store(true) }})

	if !told.Load() {
		t.Error("OnUnavailable was not called: later calls would reach a mute assistant")
	}
	if said := tts.said(); said[len(said)-1] != lineTechnical {
		t.Errorf("last words %q, want the apology", said[len(said)-1])
	}
	if !strings.Contains(logs, "reason=llm_unavailable") {
		t.Errorf("log lacks the reason\n%s", logs)
	}
}

func TestAgentSpeaksTheRefusalLine(t *testing.T) {
	tts := &recordingTTS{}
	llm := &scriptedLLM{replies: []fakeReply{{refused: true}}}
	in := &pacedInbound{frames: script(silenceFrames(100), phrase()), hangup: make(chan struct{})}
	go func() {
		time.Sleep(time.Second)
		close(in.hangup)
	}()
	runAgent(t, llm, &fakeSTT{rate: media.TelephoneRate}, tts, in, AgentOptions{})
	if !strings.Contains(strings.Join(tts.said(), "|"), lineRefusal) {
		t.Errorf("said %q, want the refusal line", tts.said())
	}
}
