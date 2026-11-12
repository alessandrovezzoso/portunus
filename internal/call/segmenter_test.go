package call

import (
	"testing"
	"time"
)

// The tests run the segmenter at 1 kHz with 20-sample frames, so one frame is
// 20 ms and every duration below is a whole number of frames.
const (
	testRate  = 1000
	testFrame = 20
)

// testSeg numbers the frames it feeds, so each frame can be filled with its
// own index and a test can tell which frames an utterance holds.
type testSeg struct {
	*segmenter
	frames int
}

func testSegmenter() *testSeg {
	return &testSeg{segmenter: newSegmenter(segmenterConfig{
		rate:         testRate,
		threshold:    0.5, // so silence is below 0.35
		silence:      100 * time.Millisecond,
		minSpeech:    60 * time.Millisecond,
		preRoll:      40 * time.Millisecond,
		maxUtterance: time.Second,
	})}
}

// feed pushes one frame per probability.
func feed(s *testSeg, probs ...float32) []*utterance {
	var out []*utterance
	frame := make([]int16, testFrame)
	for _, p := range probs {
		for i := range frame {
			frame[i] = int16(s.frames)
		}
		s.frames++
		if u := s.push(frame, p); u != nil {
			out = append(out, u)
		}
	}
	return out
}

func repeat(p float32, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = p
	}
	return out
}

func seq(parts ...[]float32) []float32 {
	var out []float32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// framesOf lists the frame indices an utterance holds, in order.
func framesOf(u *utterance) []int {
	var idx []int
	for i := 0; i < len(u.pcm); i += testFrame {
		idx = append(idx, int(u.pcm[i]))
	}
	return idx
}

func TestSegmenterIgnoresShortBursts(t *testing.T) {
	s := testSegmenter()
	// 40 ms of "speech" is under the 60 ms minimum: a click.
	if got := feed(s, seq(repeat(0, 5), repeat(1, 2), repeat(0, 10))...); len(got) != 0 {
		t.Fatalf("got %d utterances from a click, want none", len(got))
	}
}

func TestSegmenterKeepsPreRollAndEndsAfterSilence(t *testing.T) {
	s := testSegmenter()
	got := feed(s, seq(repeat(0, 5), repeat(1, 5), repeat(0, 5))...)
	if len(got) != 1 {
		t.Fatalf("got %d utterances, want 1", len(got))
	}
	u := got[0]
	// Two frames (40 ms) of pre-roll, five of speech, five of closing silence.
	want := []int{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	if f := framesOf(u); !equalInts(f, want) {
		t.Errorf("frames %v, want %v", f, want)
	}
	if u.speech != 100*time.Millisecond {
		t.Errorf("speech %v, want 100ms: pre-roll and silence must not count", u.speech)
	}
	if u.cut {
		t.Error("ended by silence, but marked as cut")
	}
}

func TestSegmenterHysteresis(t *testing.T) {
	s := testSegmenter()
	// 0.4 is below the start threshold but above the silence one, so once
	// speech has started it still counts as speech.
	got := feed(s, seq(repeat(1, 5), repeat(0.4, 10), repeat(0, 5))...)
	if len(got) != 1 {
		t.Fatalf("got %d utterances, want 1", len(got))
	}
	if got[0].speech != 300*time.Millisecond {
		t.Errorf("speech %v, want 300ms", got[0].speech)
	}
	// But 0.4 alone never starts an utterance.
	if got := feed(testSegmenter(), repeat(0.4, 30)...); len(got) != 0 {
		t.Errorf("0.4 started %d utterances, want none", len(got))
	}
}

func TestSegmenterNeedsUnbrokenSilence(t *testing.T) {
	s := testSegmenter()
	// A 60 ms pause is shorter than the 100 ms of silence that ends a phrase.
	got := feed(s, seq(repeat(1, 3), repeat(0, 3), repeat(1, 3), repeat(0, 5))...)
	if len(got) != 1 {
		t.Fatalf("got %d utterances, want 1: a short pause must not split a phrase", len(got))
	}
	if got[0].speech != 180*time.Millisecond {
		t.Errorf("speech %v, want 180ms", got[0].speech)
	}
}

func TestSegmenterCutsAtMaxLength(t *testing.T) {
	s := testSegmenter()
	// 1.2 s of continuous speech with a 1 s limit: a cut, then the rest.
	got := feed(s, seq(repeat(1, 60), repeat(0, 5))...)
	if len(got) != 2 {
		t.Fatalf("got %d utterances, want 2", len(got))
	}
	if !got[0].cut || len(got[0].pcm) != testRate {
		t.Errorf("first: cut=%v, %d samples; want a cut at %d", got[0].cut, len(got[0].pcm), testRate)
	}
	if got[1].cut || got[1].speech != 200*time.Millisecond {
		t.Errorf("second: cut=%v, speech %v; want the remaining 200ms", got[1].cut, got[1].speech)
	}
	// The second piece follows the first with no gap and no repeated audio.
	if f := framesOf(got[1]); f[0] != 50 {
		t.Errorf("second piece starts at frame %d, want 50", f[0])
	}
}

func TestSegmenterCopiesFrames(t *testing.T) {
	s := testSegmenter()
	frame := make([]int16, testFrame)
	var u *utterance
	for i := range 12 {
		p := float32(1)
		if i >= 6 {
			p = 0
		}
		for j := range frame {
			frame[j] = 7
		}
		if got := s.push(frame, p); got != nil {
			u = got
		}
		for j := range frame {
			frame[j] = -1 // the caller reuses its buffer
		}
	}
	if u == nil {
		t.Fatal("no utterance")
	}
	for i, v := range u.pcm {
		if v != 7 {
			t.Fatalf("sample %d = %d: the segmenter kept the caller's buffer", i, v)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
