package media

// silenceLevel is the amplitude under which a sample counts as silence when
// trimming: about -50 dBFS, well under quiet speech and above the noise floor
// a TTS leaves around an utterance.
const silenceLevel = 100

// TrimSilence drops the silence that leads an utterance, keeping 20 ms of it
// so the first syllable is not clipped, and the silence that trails it,
// keeping 100 ms so the last one is not either. rate is the sample rate of
// pcm. A clip that is all silence comes back empty.
func TrimSilence(pcm []int16, rate int) []int16 {
	loud := func(s int16) bool { return s > silenceLevel || s < -silenceLevel }

	first := -1
	for i, s := range pcm {
		if loud(s) {
			first = i
			break
		}
	}
	if first < 0 {
		return pcm[:0]
	}
	last := first
	for i := len(pcm) - 1; i > first; i-- {
		if loud(pcm[i]) {
			last = i
			break
		}
	}

	start := max(first-rate/50, 0)       // 20 ms
	end := min(last+1+rate/10, len(pcm)) // 100 ms
	return pcm[start:end]
}
