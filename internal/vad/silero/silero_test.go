package silero

import (
	"math/rand/v2"
	"testing"
)

func TestSilenceAndNoiseAreNotSpeech(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	frame := make([]int16, v.FrameSize())
	for i := range 30 { // about a second of digital silence
		if p := v.Process(frame); p > 0.1 {
			t.Fatalf("silence frame %d: probability %.2f, want under 0.1", i, p)
		}
	}

	// Low white noise, like a quiet phone line: still not speech.
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 30 {
		for j := range frame {
			frame[j] = int16(rng.IntN(400) - 200)
		}
		if p := v.Process(frame); p > 0.3 {
			t.Fatalf("noise frame %d: probability %.2f, want under 0.3", i, p)
		}
	}
}

func TestEachCallGetsItsOwnState(t *testing.T) {
	a, _ := New()
	b, _ := New()
	if a.model == b.model {
		t.Fatal("two detectors share one model state")
	}
}

func TestWrongFrameSizePanics(t *testing.T) {
	v, _ := New()
	defer func() {
		if recover() == nil {
			t.Error("a 20 ms frame at 8 kHz must panic, not be guessed at")
		}
	}()
	v.Process(make([]int16, 160))
}

func BenchmarkProcess(b *testing.B) {
	v, _ := New()
	frame := make([]int16, v.FrameSize())
	for b.Loop() {
		v.Process(frame)
	}
}
