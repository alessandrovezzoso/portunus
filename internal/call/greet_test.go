package call

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// fakeTTS returns a fixed tone as PCM. With hold set, the stream sends its
// audio and then stays open until the context ends, like an HTTP body whose
// request was cancelled.
type fakeTTS struct {
	rate int
	pcm  []byte
	hold bool
	err  error

	mu   sync.Mutex
	text string
}

func toneBytes(rate int, d time.Duration) []byte {
	n := int(float64(rate) * d.Seconds())
	var b bytes.Buffer
	for i := range n {
		s := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		_ = binary.Write(&b, binary.LittleEndian, s)
	}
	return b.Bytes()
}

func (f *fakeTTS) SampleRate() int { return f.rate }

func (f *fakeTTS) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.text = text
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var r io.Reader = bytes.NewReader(f.pcm)
	if f.hold {
		r = io.MultiReader(r, ctxBlocker{ctx})
	}
	return io.NopCloser(r), nil
}

type ctxBlocker struct{ ctx context.Context }

func (b ctxBlocker) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

// pacedSink stands in for diago's RTP writer: it keeps every frame and
// sleeps a little per Write, a fast-forward version of the 20 ms clock.
type pacedSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *pacedSink) Write(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	s.mu.Lock()
	s.frames = append(s.frames, bytes.Clone(b))
	s.mu.Unlock()
	return len(b), nil
}

func (s *pacedSink) audioFrames(silence byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.frames {
		if !bytes.Equal(f, bytes.Repeat([]byte{silence}, len(f))) {
			n++
		}
	}
	return n
}

// testCall returns a call whose inbound stays open until hangUp is called.
func testCall(codec sip.Codec) (c *sip.Call, out *pacedSink, hangUp func()) {
	in, inW := io.Pipe()
	out = &pacedSink{}
	return &sip.Call{ID: "test-call", Codec: codec, Inbound: in, Outbound: out}, out, func() { _ = inW.Close() }
}

func runGreet(t *testing.T, ctx context.Context, h sip.Handler, c *sip.Call) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h(ctx, c) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the greet handler did not return")
		return nil
	}
}

func TestGreetSpeaksAndHangsUp(t *testing.T) {
	tts := &fakeTTS{rate: 24000, pcm: toneBytes(24000, 500*time.Millisecond)}
	var logs bytes.Buffer
	h := Greet(tts, "Buongiorno.", GreetOptions{
		OutputBuffer: 10 * time.Second,
		Tail:         5 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	})
	c, out, hangUp := testCall(sip.CodecPCMA)
	defer hangUp()

	if err := runGreet(t, context.Background(), h, c); err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if tts.text != "Buongiorno." {
		t.Errorf("synthesized %q, want the greeting text", tts.text)
	}
	// Half a second is 25 frames of 20 ms; the resampler adds a short tail.
	if n := out.audioFrames(media.ALaw.Silence()); n < 25 || n > 28 {
		t.Errorf("%d frames of audio sent, want about 25", n)
	}
	for _, want := range []string{"call audio quality", "dropped_bytes=0", "call latency", "tts_first_audio_ms=", "first_audio_ms=", "call_id=test-call"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
}

func TestGreetStopsWhenTheCallerHangsUp(t *testing.T) {
	tts := &fakeTTS{rate: 24000, pcm: toneBytes(24000, 100*time.Millisecond), hold: true}
	h := Greet(tts, "Buongiorno.", GreetOptions{OutputBuffer: 10 * time.Second, Logger: slog.New(slog.DiscardHandler)})
	c, _, hangUp := testCall(sip.CodecPCMU)

	time.AfterFunc(50*time.Millisecond, hangUp)
	if err := runGreet(t, context.Background(), h, c); err != nil {
		t.Errorf("a hangup during the greeting must not be an error, got %v", err)
	}
}

func TestGreetStopsOnShutdown(t *testing.T) {
	tts := &fakeTTS{rate: 24000, pcm: toneBytes(24000, 100*time.Millisecond), hold: true}
	h := Greet(tts, "Buongiorno.", GreetOptions{OutputBuffer: 10 * time.Second, Logger: slog.New(slog.DiscardHandler)})
	c, _, hangUp := testCall(sip.CodecPCMU)
	defer hangUp()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := runGreet(t, ctx, h, c); err != nil {
		t.Errorf("a shutdown must not be an error, got %v", err)
	}
}

func TestGreetReportsTTSFailure(t *testing.T) {
	boom := errors.New("connection refused")
	h := Greet(&fakeTTS{rate: 24000, err: boom}, "Buongiorno.", GreetOptions{OutputBuffer: time.Second, Logger: slog.New(slog.DiscardHandler)})
	c, _, hangUp := testCall(sip.CodecPCMU)
	defer hangUp()

	if err := runGreet(t, context.Background(), h, c); !errors.Is(err, boom) {
		t.Errorf("Greet = %v, want it to wrap %v", err, boom)
	}
}

func TestLawFor(t *testing.T) {
	if lawFor(sip.CodecPCMA) != media.ALaw || lawFor(sip.CodecPCMU) != media.ULaw {
		t.Error("PCMA must map to A-law and PCMU to µ-law")
	}
}
