package media

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// frameSink records the frames it receives and cancels the run after a set
// number of them. It stands in for diago's writer; it does not block, so the
// tests run as fast as the pacer can.
type frameSink struct {
	frames [][]byte
	stopAt int
	cancel context.CancelFunc
	err    error
}

func (s *frameSink) Write(b []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.frames = append(s.frames, bytes.Clone(b))
	if len(s.frames) == s.stopAt {
		s.cancel()
	}
	return len(b), nil
}

func run(t *testing.T, p *Pacer, frames int) [][]byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &frameSink{stopAt: frames, cancel: cancel}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sink) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
	return sink.frames
}

func TestPacerSendsFullFramesAndPadsWithSilence(t *testing.T) {
	p := NewPacer(ALaw.Silence(), 10*BytesPerSecond)
	audio := bytes.Repeat([]byte{0x11}, FrameSize+40) // one frame and a quarter
	_, _ = p.Write(audio)

	frames := run(t, p, 4)
	for i, f := range frames {
		if len(f) != FrameSize {
			t.Fatalf("frame %d is %d bytes, want %d", i, len(f), FrameSize)
		}
	}
	if !bytes.Equal(frames[0], audio[:FrameSize]) {
		t.Error("frame 0 should carry the first 160 bytes of audio")
	}
	wantTail := append(bytes.Clone(audio[FrameSize:]), bytes.Repeat([]byte{0xD5}, FrameSize-40)...)
	if !bytes.Equal(frames[1], wantTail) {
		t.Error("frame 1 should carry the 40-byte tail, padded with A-law silence")
	}
	silence := bytes.Repeat([]byte{0xD5}, FrameSize)
	if !bytes.Equal(frames[2], silence) || !bytes.Equal(frames[3], silence) {
		t.Error("an empty queue must produce frames of silence")
	}

	st := p.Stats()
	if st.AudioFrames != 2 || st.PaddedFrames != 2 || st.StarvedFrames != 0 {
		t.Errorf("stats = %+v, want 2 audio and 2 padded frames, none starved", st)
	}
	if st.FirstAudio.IsZero() {
		t.Error("FirstAudio must be set once real audio is sent")
	}
}

func TestPacerCountsStarvationOnlyWhileSpeaking(t *testing.T) {
	p := NewPacer(ULaw.Silence(), 10*BytesPerSecond)
	run(t, p, 3) // idle: padding, not starvation

	p.SetSpeaking(true)
	_, _ = p.Write(bytes.Repeat([]byte{0x22}, FrameSize))
	run(t, p, 4) // one audio frame, then three starved ones

	st := p.Stats()
	if st.AudioFrames != 1 || st.StarvedFrames != 3 || st.PaddedFrames != 6 {
		t.Errorf("stats = %+v, want 1 audio, 3 starved, 6 padded", st)
	}
	if got := st.StarvedPct(); got != 75 {
		t.Errorf("StarvedPct = %v, want 75", got)
	}
}

func TestPacerDropsTheOldestOnOverflow(t *testing.T) {
	p := NewPacer(ULaw.Silence(), 2*FrameSize)
	_, _ = p.Write(bytes.Repeat([]byte{1}, FrameSize))
	_, _ = p.Write(bytes.Repeat([]byte{2}, FrameSize))
	_, _ = p.Write(bytes.Repeat([]byte{3}, FrameSize)) // pushes the 1s out

	if got := p.Len(); got != 2*FrameSize {
		t.Fatalf("Len = %d, want %d", got, 2*FrameSize)
	}
	if got := p.Stats().DroppedBytes; got != FrameSize {
		t.Errorf("DroppedBytes = %d, want %d", got, FrameSize)
	}
	frames := run(t, p, 2)
	if frames[0][0] != 2 || frames[1][0] != 3 {
		t.Errorf("sent %d then %d, want the newest audio (2 then 3)", frames[0][0], frames[1][0])
	}
}

func TestPacerFlush(t *testing.T) {
	p := NewPacer(ULaw.Silence(), 10*BytesPerSecond)
	_, _ = p.Write(bytes.Repeat([]byte{7}, 3*FrameSize))
	if n := p.Flush(); n != 3*FrameSize {
		t.Errorf("Flush = %d, want %d", n, 3*FrameSize)
	}
	if p.Len() != 0 {
		t.Error("the queue must be empty after Flush")
	}
	if got := p.Stats().FlushedBytes; got != 3*FrameSize {
		t.Errorf("FlushedBytes = %d, want %d", got, 3*FrameSize)
	}
}

func TestPacerStopsOnWriterError(t *testing.T) {
	p := NewPacer(ULaw.Silence(), FrameSize)
	boom := errors.New("socket closed")
	err := p.Run(context.Background(), &frameSink{err: boom})
	if !errors.Is(err, boom) {
		t.Errorf("Run = %v, want %v", err, boom)
	}
}
