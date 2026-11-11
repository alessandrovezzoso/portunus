package media

import "math"

// Resampler converts a stream of 16-bit PCM from one sample rate to another,
// for any rational ratio (24 kHz to 8 kHz for the TTS, 8 kHz to 16 kHz for an
// STT later). It keeps state between calls, so audio can arrive in pieces of
// any size, as it does from a streaming TTS.
//
// It works as a polyphase FIR filter: conceptually it inserts up-1 zeros
// between input samples, low-pass filters the result, and keeps every
// down-th sample, but it only computes the samples it keeps. The low-pass
// filter is what stops frequencies above the new Nyquist limit from folding
// back into the voice band as aliasing, which on a phone sounds like a hiss.
type Resampler struct {
	up, down int         // the ratio outRate/inRate, reduced: out = in * up / down
	phases   [][]float64 // the filter split into up phases of equal length
	hist     []float64   // input samples still needed by future outputs
	t        int         // next output position, in the upsampled domain, relative to hist[0]
}

// The filter is a Blackman-windowed sinc. The cutoff sits at the lower
// Nyquist frequency. The Blackman window attenuates the stopband by about
// 74 dB, and its transition band is about 5.5/N of the upsampled rate wide,
// so N is chosen to make the transition 15% of the lower rate. At 24 kHz to
// 8 kHz that means flat up to 3.4 kHz, the top of the telephone band, and
// silent from 4.6 kHz.
const transitionFraction = 0.15

// NewResampler returns a resampler from inRate to outRate, both in Hz.
func NewResampler(inRate, outRate int) *Resampler {
	g := gcd(inRate, outRate)
	up, down := outRate/g, inRate/g

	upRate := float64(inRate * up)
	lower := float64(min(inRate, outRate))
	cutoff := lower / 2 / upRate // cycles per sample, at the upsampled rate

	n := int(math.Ceil(5.5 * upRate / (transitionFraction * lower)))
	perPhase := (n + up - 1) / up
	n = perPhase * up

	h := make([]float64, n)
	center := float64(n-1) / 2
	sum := 0.0
	for i := range h {
		x := float64(i) - center
		sinc := 2 * cutoff
		if x != 0 {
			sinc = math.Sin(2*math.Pi*cutoff*x) / (math.Pi * x)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) + 0.08*math.Cos(4*math.Pi*float64(i)/float64(n-1))
		h[i] = sinc * w
		sum += h[i]
	}
	// Normalise the DC gain to up: inserting zeros divides the energy by up,
	// and this gives it back, so a tone keeps its amplitude across the change.
	for i := range h {
		h[i] *= float64(up) / sum
	}

	phases := make([][]float64, up)
	for p := range phases {
		phases[p] = make([]float64, perPhase)
		for k := range perPhase {
			phases[p][k] = h[p+k*up]
		}
	}

	return &Resampler{
		up:     up,
		down:   down,
		phases: phases,
		// Start with a history of silence, so the first outputs are defined.
		hist: make([]float64, perPhase-1),
		t:    (perPhase - 1) * up,
	}
}

// Process resamples the next piece of input and appends the output to dst.
func (r *Resampler) Process(dst []int16, in []int16) []int16 {
	for _, s := range in {
		r.hist = append(r.hist, float64(s))
	}
	perPhase := len(r.phases[0])

	for {
		i := r.t / r.up // the newest input sample this output depends on
		if i >= len(r.hist) {
			break
		}
		taps := r.phases[r.t%r.up]
		acc := 0.0
		for k, h := range taps {
			acc += h * r.hist[i-k]
		}
		dst = append(dst, clamp16(acc))
		r.t += r.down
	}

	// Drop the inputs no future output will reach, keeping perPhase-1 of
	// history behind the next one.
	if drop := r.t/r.up - (perPhase - 1); drop > 0 {
		r.hist = append(r.hist[:0], r.hist[drop:]...)
		r.t -= drop * r.up
	}
	return dst
}

// Flush pushes silence through the filter to emit the tail still held in its
// history, and appends it to dst. Call it once, at the end of a stream.
func (r *Resampler) Flush(dst []int16) []int16 {
	return r.Process(dst, make([]int16, len(r.phases[0])))
}

func clamp16(v float64) int16 {
	v = math.Round(v)
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(v)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
