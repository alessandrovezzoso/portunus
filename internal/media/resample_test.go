package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"slices"
	"testing"
	"testing/iotest"
)

// tone returns n samples of a sine at freq Hz, sampled at rate Hz.
func tone(freq float64, rate, n int, amplitude float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amplitude * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

// level measures the amplitude of the component at freq Hz in samples, with
// the Goertzel algorithm: a single-bin Fourier transform.
func level(samples []int16, freq float64, rate int) float64 {
	w := 2 * math.Pi * freq / float64(rate)
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for _, x := range samples {
		s := float64(x) + coeff*s1 - s2
		s2, s1 = s1, s
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	return 2 * math.Sqrt(power) / float64(len(samples))
}

func db(ratio float64) float64 { return 20 * math.Log10(ratio) }

// steady drops the filter's start-up transient, so only the settled signal
// is measured.
func steady(s []int16) []int16 { return s[len(s)/4 : len(s)*3/4] }

func TestResamplerKeepsTheVoiceBand(t *testing.T) {
	const amp = 10000.0
	for _, freq := range []float64{300, 1000, 3000, 3400} {
		in := tone(freq, 24000, 24000, amp)
		out := NewResampler(24000, 8000).Process(nil, in)

		if want := len(in) / 3; abs(len(out)-want) > 2 {
			t.Errorf("%v Hz: %d output samples, want about %d", freq, len(out), want)
		}
		got := level(steady(out), freq, 8000)
		if d := db(got / amp); math.Abs(d) > 0.5 {
			t.Errorf("%v Hz: level changed by %.2f dB, want within 0.5 dB", freq, d)
		}
	}
}

func TestResamplerRemovesAliases(t *testing.T) {
	const amp = 10000.0
	// Above the new Nyquist limit of 4 kHz these tones would fold back into
	// the voice band: 5 kHz to 3 kHz, 7 kHz to 1 kHz.
	for _, c := range []struct{ freq, alias float64 }{{5000, 3000}, {7000, 1000}} {
		in := tone(c.freq, 24000, 24000, amp)
		out := NewResampler(24000, 8000).Process(nil, in)
		got := level(steady(out), c.alias, 8000)
		if d := db(got / amp); d > -40 {
			t.Errorf("%v Hz folds to %v Hz at %.1f dB, want below -40 dB", c.freq, c.alias, d)
		}
	}
}

func TestResamplerUpsamples(t *testing.T) {
	const amp = 10000.0
	in := tone(1000, 8000, 8000, amp)
	out := NewResampler(8000, 16000).Process(nil, in)
	if want := 2 * len(in); abs(len(out)-want) > 2 {
		t.Errorf("%d output samples, want about %d", len(out), want)
	}
	if d := db(level(steady(out), 1000, 16000) / amp); math.Abs(d) > 0.5 {
		t.Errorf("1 kHz level changed by %.2f dB through 8→16 kHz", d)
	}
}

func TestResamplerChunkingDoesNotMatter(t *testing.T) {
	in := tone(1234, 24000, 9000, 8000)

	whole := NewResampler(24000, 8000)
	want := whole.Flush(whole.Process(nil, in))

	pieces := NewResampler(24000, 8000)
	var got []int16
	for rest, size := in, 1; len(rest) > 0; size = size%997 + 7 {
		n := min(size, len(rest))
		got = pieces.Process(got, rest[:n])
		rest = rest[n:]
	}
	got = pieces.Flush(got)

	if !slices.Equal(got, want) {
		t.Errorf("chunked output differs from one-shot output (%d vs %d samples)", len(got), len(want))
	}
}

func TestPCM16ReaderCarriesSplitSamples(t *testing.T) {
	want := []int16{1, -2, 300, -32768, 32767, 0, 12345}
	var raw bytes.Buffer
	for _, s := range want {
		_ = binary.Write(&raw, binary.LittleEndian, s)
	}

	// OneByteReader returns a single byte per Read, splitting every sample.
	r := NewPCM16Reader(iotest.OneByteReader(bytes.NewReader(raw.Bytes())))
	var got []int16
	buf := make([]int16, 3)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEncoderProducesTelephoneRate(t *testing.T) {
	in := tone(1000, 24000, 24000, 8000) // one second at 24 kHz
	enc := NewEncoder(24000, ALaw)
	out := enc.Encode(in)
	out = append(out, enc.Flush()...)

	// One second of telephone audio is 8000 bytes, plus the filter's tail.
	if len(out) < 8000 || len(out) > 8000+200 {
		t.Errorf("%d bytes for one second, want about 8000", len(out))
	}
	decoded := ALaw.Decode(nil, out)
	if d := db(level(steady(decoded), 1000, 8000) / 8000); math.Abs(d) > 1 {
		t.Errorf("1 kHz level changed by %.2f dB through the encoder", d)
	}
}
