package sip

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/diago/media"
)

func TestMaskNumber(t *testing.T) {
	tests := map[string]string{
		"+390212345678": "***678",
		"0212345678":    "***678",
		"1234":          "***234",
		"123":           "***",
		"12":            "***",
		"":              "***",
		"anonymous":     "***ous",
	}
	for in, want := range tests {
		if got := MaskNumber(in); got != want {
			t.Errorf("MaskNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodecOf(t *testing.T) {
	if c, err := codecOf(media.CodecAudioAlaw); err != nil || c != CodecPCMA {
		t.Errorf("A-law: got %q, %v", c, err)
	}
	if c, err := codecOf(media.CodecAudioUlaw); err != nil || c != CodecPCMU {
		t.Errorf("µ-law: got %q, %v", c, err)
	}
	if _, err := codecOf(media.CodecAudioOpus); err == nil {
		t.Error("Opus: expected an error, the codecs are pinned to G.711")
	}
}

func TestAdmitRespectsTheLimit(t *testing.T) {
	s := &Server{opts: Options{MaxConcurrentCalls: 2}}

	for i := range 2 {
		if !s.admit() {
			t.Fatalf("call %d must be admitted", i+1)
		}
	}
	if s.admit() {
		t.Fatal("a third call must be refused")
	}
	s.release()
	if !s.admit() {
		t.Fatal("a freed slot must be reusable")
	}
	s.release()
	s.release()
	if s.active != 0 {
		t.Errorf("active = %d after releasing everything, want 0", s.active)
	}
}

func TestAdmitRefusesWhileClosing(t *testing.T) {
	s := &Server{opts: Options{MaxConcurrentCalls: 2}}
	s.drainCalls() // nothing active: returns at once and marks the server closing

	if s.admit() {
		t.Error("no call may be admitted once shutdown has started")
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), 0) {
		t.Error("a zero delay must report that it elapsed")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("a short delay must report that it elapsed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("a cancelled context must cut the wait short")
	}
	if sleepCtx(ctx, 0) {
		t.Error("a cancelled context must win even with no delay")
	}
}

func TestCountingReaderAndWriter(t *testing.T) {
	in := &countingReader{r: &scriptedReader{frames: [][]byte{frame(1), {}, frame(2)}}}
	buf := make([]byte, 200)
	for range 3 {
		_, _ = in.Read(buf)
	}
	if got := in.frames.Load(); got != 2 {
		t.Errorf("frames in = %d, want 2 (an empty read is not a frame)", got)
	}

	var sink bytes.Buffer
	out := &countingWriter{w: &sink}
	_, _ = out.Write(frame(1))
	_, _ = out.Write(frame(2))
	if got := out.frames.Load(); got != 2 {
		t.Errorf("frames out = %d, want 2", got)
	}
}

func TestReady(t *testing.T) {
	s := &Server{}
	if err := s.ready(context.Background()); err != nil {
		t.Errorf("no Ready function must mean ready, got %v", err)
	}

	s.opts.Ready = func(context.Context) error { return errors.New("STT down") }
	if err := s.ready(context.Background()); err == nil {
		t.Error("an error from Ready must refuse the call")
	}

	// Ready gets a deadline, so a slow check cannot keep the caller waiting.
	s.opts.Ready = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	if err := s.ready(context.Background()); err == nil || time.Since(start) > 2*readyTimeout {
		t.Errorf("a stuck Ready returned %v after %v", err, time.Since(start))
	}
}
