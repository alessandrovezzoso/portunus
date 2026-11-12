package media

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
)

func TestDecoderAt8kHzIsPlainG711(t *testing.T) {
	payload := []byte{0x00, 0x7F, 0x80, 0xFF, 0x55}
	for _, law := range []Law{ULaw, ALaw} {
		got := NewDecoder(law, TelephoneRate).Decode(payload)
		want := law.Decode(nil, payload)
		if len(got) != len(want) {
			t.Fatalf("%v: %d samples, want %d", law, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%v sample %d = %d, want %d", law, i, got[i], want[i])
			}
		}
	}
}

func TestDecoderUpsamplesATone(t *testing.T) {
	// A 1 kHz tone at 8 kHz, through µ-law, must come out at 16 kHz with
	// twice the samples and about the same amplitude.
	const freq, amp = 1000.0, 8000.0
	pcm := make([]int16, 8000) // one second
	for i := range pcm {
		pcm[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(i)/TelephoneRate))
	}
	payload := ULaw.Encode(nil, pcm)

	d := NewDecoder(ULaw, 16000)
	var out []int16
	for i := 0; i < len(payload); i += FrameSize { // one RTP payload at a time
		out = append(out, d.Decode(payload[i:i+FrameSize])...)
	}
	if n := len(out); n < 15900 || n > 16000 {
		t.Fatalf("%d samples out, want about 16000", n)
	}
	// Skip the filter's start-up and compare the peak with the input's.
	peak := 0.0
	for _, s := range out[1000:] {
		peak = math.Max(peak, math.Abs(float64(s)))
	}
	if peak < 0.9*amp || peak > 1.1*amp {
		t.Errorf("peak %.0f, want about %.0f", peak, amp)
	}
}

func TestToFloat32(t *testing.T) {
	got := ToFloat32(nil, []int16{0, -32768, 16384, 32767})
	want := []float32{0, -1, 0.5, 32767.0 / 32768}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
	// It appends, so a buffer can be reused across frames.
	if got := ToFloat32(make([]float32, 0, 8), []int16{1}); len(got) != 1 {
		t.Errorf("append to an empty buffer: len %d", len(got))
	}
}

func TestEncodeWAV(t *testing.T) {
	got := EncodeWAV([]int16{1, -2}, 16000)
	// The canonical header for 4 bytes of 16 kHz mono 16-bit PCM, then the
	// two samples in little endian.
	want, _ := hex.DecodeString("" +
		"52494646" + "28000000" + "57415645" + // "RIFF", 36+4, "WAVE"
		"666d7420" + "10000000" + "0100" + "0100" + // "fmt ", 16, PCM, mono
		"803e0000" + "007d0000" + "0200" + "1000" + // 16000 Hz, 32000 B/s, block 2, 16 bits
		"64617461" + "04000000" + // "data", 4
		"0100" + "feff") // 1, -2
	if !bytes.Equal(got, want) {
		t.Errorf("EncodeWAV =\n%x\nwant\n%x", got, want)
	}
}
