package call

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// scriptedInbound plays a fixed list of RTP payloads, then reports the
// hangup with io.EOF. With hold set, it waits for hold to close first, like
// a caller who stays on the line in silence.
type scriptedInbound struct {
	frames [][]byte
	next   int
	hold   <-chan struct{}
	reads  atomic.Int64
}

func (s *scriptedInbound) Read(p []byte) (int, error) {
	if s.next < len(s.frames) {
		n := copy(p, s.frames[s.next])
		s.next++
		s.reads.Add(1)
		return n, nil
	}
	if s.hold != nil {
		<-s.hold
	}
	return 0, io.EOF
}

// The caller's audio, as µ-law RTP payloads: a 1 kHz tone stands in for
// speech, and encoded silence for the pauses.
func toneFrames(n int) [][]byte {
	pcm := make([]int16, media.FrameSize)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/media.TelephoneRate))
	}
	frame := media.ULaw.Encode(nil, pcm)
	out := make([][]byte, n)
	for i := range out {
		out[i] = frame
	}
	return out
}

func silenceFrames(n int) [][]byte {
	frame := bytes.Repeat([]byte{media.ULaw.Silence()}, media.FrameSize)
	out := make([][]byte, n)
	for i := range out {
		out[i] = frame
	}
	return out
}

func script(parts ...[][]byte) [][]byte {
	var out [][]byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// loudVAD hears speech in any loud frame. At 8 kHz with 160-sample frames,
// one VAD frame is exactly one RTP payload, which keeps the scripts simple.
type loudVAD struct{}

func (loudVAD) SampleRate() int { return media.TelephoneRate }
func (loudVAD) FrameSize() int  { return media.FrameSize }
func (loudVAD) Process(frame []int16) float32 {
	for _, s := range frame {
		if s > 1000 || s < -1000 {
			return 1
		}
	}
	return 0
}

func newLoudVAD() (VAD, error) { return loudVAD{}, nil }

// fakeSTT answers "frase N" for the Nth request. With block set it never
// answers and waits for its context to end; with err set it always fails.
type fakeSTT struct {
	rate  int
	delay time.Duration
	block bool
	err   error

	mu    sync.Mutex
	sizes []int // the length of each pcm received
}

func (f *fakeSTT) SampleRate() int { return f.rate }

func (f *fakeSTT) Transcribe(ctx context.Context, pcm []int16) (string, error) {
	f.mu.Lock()
	f.sizes = append(f.sizes, len(pcm))
	n := len(f.sizes)
	f.mu.Unlock()

	switch {
	case f.block:
		<-ctx.Done()
		return "", ctx.Err()
	case f.err != nil:
		return "", f.err
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return fmt.Sprintf("frase %d", n), nil
}

func (f *fakeSTT) requests() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.sizes...)
}

// runListen runs one scripted call and returns the log it wrote, what it
// sent, and the handler's error.
func runListen(t *testing.T, stt STT, in *scriptedInbound, level slog.Level, opts ListenOptions) (logText string, out *pacedSink, err error) {
	t.Helper()
	var logs bytes.Buffer
	opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level}))
	if opts.VADSilence == 0 {
		opts.VADSilence = 200 * time.Millisecond // ten frames
	}
	out = &pacedSink{}
	h := Listen(stt, newLoudVAD, opts)

	done := make(chan error, 1)
	go func() {
		done <- h(context.Background(), &sip.Call{ID: "test-call", Codec: sip.CodecPCMU, Inbound: in, Outbound: out})
	}()
	select {
	case err = <-done:
		return logs.String(), out, err
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return")
		return "", nil, nil
	}
}

func TestListenTranscribesEachUtterance(t *testing.T) {
	stt := &fakeSTT{rate: media.TelephoneRate}
	in := &scriptedInbound{frames: script(
		silenceFrames(10), toneFrames(20), silenceFrames(15),
		toneFrames(20), silenceFrames(15),
	)}
	logs, _, err := runListen(t, stt, in, slog.LevelDebug, ListenOptions{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if n := len(stt.requests()); n != 2 {
		t.Fatalf("%d transcriptions, want 2\n%s", n, logs)
	}
	for _, want := range []string{
		`msg=utterance call_id=test-call turn=1 speech_ms=400 stt_ms=`,
		`msg=utterance call_id=test-call turn=2 speech_ms=400 stt_ms=`,
		`chars=7`,
		`msg=transcript call_id=test-call turn=1 text="frase 1"`,
		`msg="call latency" call_id=test-call turns=2 stt_ms_avg=`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q\n%s", want, logs)
		}
	}
}

func TestListenKeepsTranscriptsOutOfInfoLogs(t *testing.T) {
	stt := &fakeSTT{rate: media.TelephoneRate}
	in := &scriptedInbound{frames: script(toneFrames(20), silenceFrames(15))}
	logs, _, _ := runListen(t, stt, in, slog.LevelInfo, ListenOptions{})
	if !strings.Contains(logs, "msg=utterance") {
		t.Fatalf("no utterance logged\n%s", logs)
	}
	if strings.Contains(logs, "frase") {
		t.Errorf("transcript text in an info-level log:\n%s", logs)
	}
}

func TestListenResamplesForTheSTT(t *testing.T) {
	stt := &fakeSTT{rate: 16000}
	// 200 ms of lead-in silence (all of it becomes pre-roll), 400 ms of
	// speech, 200 ms of closing silence: 800 ms in all.
	in := &scriptedInbound{frames: script(silenceFrames(10), toneFrames(20), silenceFrames(15))}
	if logs, _, err := runListen(t, stt, in, slog.LevelDebug, ListenOptions{}); err != nil {
		t.Fatalf("handler: %v\n%s", err, logs)
	}
	req := stt.requests()
	if len(req) != 1 {
		t.Fatalf("%d transcriptions, want 1", len(req))
	}
	if req[0] < 12700 || req[0] > 12900 {
		t.Errorf("STT got %d samples, want about 12800 (800 ms at 16 kHz)", req[0])
	}
}

func TestListenNeverWaitsForASlowSTT(t *testing.T) {
	stt := &fakeSTT{rate: media.TelephoneRate, block: true}
	var frames [][]byte
	for range 6 {
		frames = append(frames, script(toneFrames(20), silenceFrames(12))...)
	}
	in := &scriptedInbound{frames: frames}
	logs, _, err := runListen(t, stt, in, slog.LevelDebug, ListenOptions{drain: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	// The whole call was read even though the STT never answered.
	if got := in.reads.Load(); got != int64(len(frames)) {
		t.Errorf("read %d of %d frames: the STT held up the inbound loop", got, len(frames))
	}
	if !strings.Contains(logs, "utterance dropped") {
		t.Errorf("a full queue must drop and log\n%s", logs)
	}
	if !strings.Contains(logs, "turns=0") || !strings.Contains(logs, "abandoned_utterances=") {
		t.Errorf("summary must report no turns and the abandoned ones\n%s", logs)
	}
}

func TestListenFinishesTheLastUtteranceAfterAHangup(t *testing.T) {
	// The caller hangs up right after the phrase: it is still transcribed.
	stt := &fakeSTT{rate: media.TelephoneRate, delay: 100 * time.Millisecond}
	in := &scriptedInbound{frames: script(toneFrames(20), silenceFrames(10))}
	logs, _, err := runListen(t, stt, in, slog.LevelDebug, ListenOptions{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(logs, `text="frase 1"`) {
		t.Errorf("the last phrase was lost at the hangup\n%s", logs)
	}
}

func TestListenSurvivesSTTFailures(t *testing.T) {
	stt := &fakeSTT{rate: media.TelephoneRate, err: errors.New("connection refused")}
	in := &scriptedInbound{frames: script(toneFrames(20), silenceFrames(15), toneFrames(20), silenceFrames(15))}
	logs, _, err := runListen(t, stt, in, slog.LevelDebug, ListenOptions{})
	if err != nil {
		t.Fatalf("an STT failure must not fail the call: %v", err)
	}
	if strings.Count(logs, "transcription failed") != 2 || !strings.Contains(logs, "stt_failed=2") {
		t.Errorf("want two failures logged and counted\n%s", logs)
	}
}

func TestListenSendsSilence(t *testing.T) {
	hold := make(chan struct{})
	in := &scriptedInbound{hold: hold}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(hold)
	}()
	_, out, _ := runListen(t, &fakeSTT{rate: media.TelephoneRate}, in, slog.LevelDebug, ListenOptions{})

	out.mu.Lock()
	defer out.mu.Unlock()
	if len(out.frames) == 0 {
		t.Fatal("no RTP sent: the far end could think the line is dead")
	}
	for i, f := range out.frames {
		if len(f) != media.FrameSize || bytes.Count(f, []byte{media.ULaw.Silence()}) != len(f) {
			t.Fatalf("frame %d is not a full frame of µ-law silence", i)
		}
	}
}
