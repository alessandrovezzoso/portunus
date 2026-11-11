package media

import (
	"encoding/binary"
	"os"
	"testing"
)

// The golden files hold ffmpeg's decoding of every G.711 code, 0x00 to 0xFF,
// as 16-bit little-endian PCM. They were generated once with:
//
//	ffmpeg -f mulaw -ar 8000 -ac 1 -i codes_0_255.bin -f s16le ulaw_decoded_ffmpeg.s16le
//	ffmpeg -f alaw  -ar 8000 -ac 1 -i codes_0_255.bin -f s16le alaw_decoded_ffmpeg.s16le
//
// Decoding is unambiguous (each code has exactly one reconstruction level), so
// ffmpeg is a fair reference for it. Encoding is checked by the properties
// below rather than against ffmpeg, whose encoder rounds differently at some
// segment boundaries than the ITU reference does.
func readGolden(t *testing.T, name string) []int16 {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if len(raw) != 512 {
		t.Fatalf("%s: %d bytes, want 512 (256 samples)", name, len(raw))
	}
	out := make([]int16, 256)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out
}

func allCodes() []byte {
	codes := make([]byte, 256)
	for i := range codes {
		codes[i] = byte(i)
	}
	return codes
}

func TestDecodeMatchesFFmpeg(t *testing.T) {
	for _, tc := range []struct {
		law    Law
		golden string
	}{
		{ULaw, "ulaw_decoded_ffmpeg.s16le"},
		{ALaw, "alaw_decoded_ffmpeg.s16le"},
	} {
		t.Run(tc.law.String(), func(t *testing.T) {
			want := readGolden(t, tc.golden)
			got := tc.law.Decode(nil, allCodes())
			for code := range 256 {
				if got[code] != want[code] {
					t.Errorf("Decode(0x%02X) = %d, ffmpeg says %d", code, got[code], want[code])
				}
			}
		})
	}
}

func TestEncodeInvertsDecode(t *testing.T) {
	for _, law := range []Law{ULaw, ALaw} {
		t.Run(law.String(), func(t *testing.T) {
			for _, code := range allCodes() {
				// µ-law has two codes for zero, +0 (0xFF) and -0 (0x7F); zero
				// encodes as +0.
				if law == ULaw && code == 0x7F {
					continue
				}
				level := law.Decode(nil, []byte{code})[0]
				if back := law.Encode(nil, []int16{level})[0]; back != code {
					t.Errorf("Encode(Decode(0x%02X) = %d) = 0x%02X", code, level, back)
				}
			}
		})
	}
}

func TestEncodeSilence(t *testing.T) {
	for _, law := range []Law{ULaw, ALaw} {
		if got := law.Encode(nil, []int16{0})[0]; got != law.Silence() {
			t.Errorf("%v: Encode(0) = 0x%02X, Silence() = 0x%02X", law, got, law.Silence())
		}
	}
	if ULaw.Silence() != 0xFF || ALaw.Silence() != 0xD5 {
		t.Error("silence bytes must be 0xFF for µ-law and 0xD5 for A-law")
	}
}

func TestEncodeIsMonotonicAndAccurate(t *testing.T) {
	for _, law := range []Law{ULaw, ALaw} {
		t.Run(law.String(), func(t *testing.T) {
			prev := int16(-32768)
			for s := -32768; s <= 32767; s++ {
				in := int16(s)
				out := law.Decode(nil, law.Encode(nil, []int16{in}))[0]
				if out < prev {
					t.Fatalf("not monotonic: sample %d decodes to %d, below %d", s, out, prev)
				}
				prev = out
				// Companding keeps the error proportional to the signal: a
				// sixteenth of the magnitude, plus the smallest step.
				if err, bound := abs(int(out)-s), abs(s)/16+16; err > bound {
					t.Fatalf("sample %d decodes to %d: error %d exceeds %d", s, out, err, bound)
				}
			}
		})
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
