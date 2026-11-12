package call

import "time"

// The segmenter turns a stream of VAD frames, each with its speech
// probability, into utterances: the stretches of audio worth sending to the
// STT. It is pure logic, with no clock and no I/O, so tests can script it
// frame by frame.
//
// Its rules follow Silero's own guidance:
//   - Speech starts when the probability reaches the threshold, but it only
//     ends once the probability stays below a lower one. The gap (hysteresis)
//     stops a word that dips for a frame from being cut in two.
//   - The utterance ends after a run of silence. That run is the main latency
//     knob: every turn waits for it before the STT even starts.
//   - Audio from just before the detected start is kept (pre-roll), because
//     the VAD needs a frame or two of a word before it is sure, and the STT
//     needs the start of that word.
//   - Very short bursts are dropped: a cough or a click is not a phrase.
//   - A long monologue is cut at a maximum length, which must fit the STT's
//     input window.

const (
	// hysteresis is how far below the start threshold the probability must
	// fall to count as silence: 0.15 is Silero's recommendation.
	hysteresis = 0.15

	defaultThreshold    = 0.5
	defaultSilence      = 700 * time.Millisecond
	defaultMinSpeech    = 250 * time.Millisecond
	defaultPreRoll      = 300 * time.Millisecond
	defaultMaxUtterance = 25 * time.Second
)

// segmenterConfig sets the rules. Durations are converted to samples once, so
// the per-frame code only counts.
type segmenterConfig struct {
	rate         int     // samples per second of the frames pushed in
	threshold    float32 // speech starts at or above this probability
	silence      time.Duration
	minSpeech    time.Duration
	preRoll      time.Duration
	maxUtterance time.Duration
}

// utterance is one phrase, ready for the STT.
type utterance struct {
	pcm []int16
	// speech runs from the detected start to the end of the last frame that
	// still counted as speech: the pre-roll and the closing silence are not
	// part of it.
	speech time.Duration
	// cut reports that the length limit ended the utterance, not silence:
	// the caller was still talking.
	cut bool
}

type segmenter struct {
	cfg segmenterConfig
	off float32 // the probability below which a frame counts as silence

	silenceSamples, minSpeechSamples, preRollSamples, maxSamples int

	pre []int16 // the most recent audio while idle, for the pre-roll

	inSpeech   bool
	pcm        []int16 // the utterance so far, pre-roll included
	start      int     // where the detected speech starts within pcm
	lastVoiced int     // end of the last voiced frame within pcm
	silent     int     // samples of silence since the last voiced frame
}

func newSegmenter(cfg segmenterConfig) *segmenter {
	samples := func(d time.Duration) int { return int(d.Seconds() * float64(cfg.rate)) }
	off := cfg.threshold - hysteresis
	if off < 0.01 {
		off = 0.01
	}
	return &segmenter{
		cfg:              cfg,
		off:              off,
		silenceSamples:   samples(cfg.silence),
		minSpeechSamples: samples(cfg.minSpeech),
		preRollSamples:   samples(cfg.preRoll),
		maxSamples:       samples(cfg.maxUtterance),
	}
}

// push adds one frame and its speech probability. It returns an utterance
// when this frame completes one, and nil otherwise. The frame is copied, so
// the caller may reuse its buffer.
func (s *segmenter) push(frame []int16, prob float32) *utterance {
	if !s.inSpeech {
		if prob < s.cfg.threshold {
			s.remember(frame)
			return nil
		}
		// Speech starts: the utterance opens with the pre-roll.
		s.inSpeech = true
		s.pcm = append(s.pcm[:0], s.pre...)
		s.start = len(s.pcm)
		s.pre = s.pre[:0]
		s.silent = 0
	}

	s.pcm = append(s.pcm, frame...)
	if prob >= s.off {
		s.lastVoiced = len(s.pcm)
		s.silent = 0
	} else {
		s.silent += len(frame)
	}

	switch {
	case s.silent >= s.silenceSamples:
		return s.finish(false)
	case len(s.pcm)-s.start >= s.maxSamples:
		return s.finish(true)
	}
	return nil
}

// finish closes the current utterance. A cut keeps the segmenter in speech,
// because the caller is still talking: the next frame opens a new utterance
// right away, with no pre-roll, since nothing was missed.
func (s *segmenter) finish(cut bool) *utterance {
	voiced := s.lastVoiced - s.start
	pcm := s.pcm
	s.pcm = nil // the utterance owns this buffer now
	s.start, s.lastVoiced, s.silent = 0, 0, 0
	s.inSpeech = cut

	if voiced < s.minSpeechSamples && !cut {
		return nil // a cough or a click, not a phrase
	}
	return &utterance{
		pcm:    pcm,
		speech: time.Duration(float64(voiced) / float64(s.cfg.rate) * float64(time.Second)),
		cut:    cut,
	}
}

// reset forgets the utterance in progress and the pre-roll, as if the
// segmenter had just been created. The ear calls it while the assistant is
// talking, so nothing heard then leaks into the next utterance.
func (s *segmenter) reset() {
	s.inSpeech = false
	s.pcm = s.pcm[:0]
	s.pre = s.pre[:0]
	s.start, s.lastVoiced, s.silent = 0, 0, 0
}

// remember keeps the last preRollSamples of idle audio.
func (s *segmenter) remember(frame []int16) {
	s.pre = append(s.pre, frame...)
	if extra := len(s.pre) - s.preRollSamples; extra > 0 {
		s.pre = s.pre[:copy(s.pre, s.pre[extra:])]
	}
}
