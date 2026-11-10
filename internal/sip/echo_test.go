package sip

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// scriptedReader returns one scripted payload per Read, then err.
type scriptedReader struct {
	frames [][]byte
	err    error
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.frames) == 0 {
		return 0, r.err
	}
	n := copy(p, r.frames[0])
	r.frames = r.frames[1:]
	return n, nil
}

// recordingWriter keeps a copy of every frame written to it.
type recordingWriter struct {
	frames [][]byte
	err    error
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.frames = append(w.frames, bytes.Clone(p))
	return len(p), nil
}

func frame(b byte) []byte { return bytes.Repeat([]byte{b}, 160) }

func TestEchoSendsFramesBackUntilEOF(t *testing.T) {
	in := &scriptedReader{
		frames: [][]byte{frame(0xD5), {}, frame(0x01), frame(0x02)}, // {} is a keepalive
		err:    io.EOF,
	}
	out := &recordingWriter{}

	if err := Echo(context.Background(), &Call{Inbound: in, Outbound: out}); err != nil {
		t.Fatalf("Echo: %v", err)
	}

	want := [][]byte{frame(0xD5), frame(0x01), frame(0x02)}
	if len(out.frames) != len(want) {
		t.Fatalf("echoed %d frames, want %d (the empty keepalive must be skipped)", len(out.frames), len(want))
	}
	for i := range want {
		if !bytes.Equal(out.frames[i], want[i]) {
			t.Errorf("frame %d differs from what was received", i)
		}
	}
}

func TestEchoTreatsClosedMediaAsAHangup(t *testing.T) {
	in := &scriptedReader{frames: [][]byte{frame(1)}, err: io.EOF}
	out := &recordingWriter{err: net.ErrClosed}

	if err := Echo(context.Background(), &Call{Inbound: in, Outbound: out}); err != nil {
		t.Errorf("Echo with a closed writer: got %v, want nil", err)
	}
}

func TestEchoReportsUnexpectedErrors(t *testing.T) {
	boom := errors.New("boom")

	err := Echo(context.Background(), &Call{Inbound: &scriptedReader{err: boom}, Outbound: &recordingWriter{}})
	if !errors.Is(err, boom) {
		t.Errorf("read error: got %v, want it to wrap %v", err, boom)
	}

	in := &scriptedReader{frames: [][]byte{frame(1)}, err: io.EOF}
	err = Echo(context.Background(), &Call{Inbound: in, Outbound: &recordingWriter{err: boom}})
	if !errors.Is(err, boom) {
		t.Errorf("write error: got %v, want it to wrap %v", err, boom)
	}
}

// endlessReader returns a frame on every Read and never ends by itself, like a
// call that is still up.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) { return copy(p, frame(7)), nil }

// cancellingWriter cancels the call after a few frames, as a shutdown would.
type cancellingWriter struct {
	after  int
	cancel context.CancelFunc
	n      int
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n == w.after {
		w.cancel()
	}
	return len(p), nil
}

func TestEchoStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &cancellingWriter{after: 3, cancel: cancel}

	done := make(chan error, 1)
	go func() { done <- Echo(ctx, &Call{Inbound: endlessReader{}, Outbound: out}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Echo after cancel: got %v, want nil", err)
		}
		if out.n != 3 {
			t.Errorf("wrote %d frames, want Echo to stop right after the cancel at frame 3", out.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Echo did not return after its context was cancelled")
	}
}
