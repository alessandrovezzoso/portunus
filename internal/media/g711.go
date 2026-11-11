// Package media does every conversion between sample formats and rates:
// G.711 encoding, resampling, raw PCM framing and the outbound pacer. No
// other package touches samples, so a bug in audio quality has one home.
//
// Everything here is a pure function or a small self-contained type, which
// is what makes it testable against golden samples.
package media

// Law is a G.711 companding law. The SIP leg negotiates one per call.
type Law int

const (
	ULaw Law = iota // µ-law (PCMU), North America and Japan
	ALaw            // A-law (PCMA), Europe and most of the world
)

func (l Law) String() string {
	if l == ALaw {
		return "A-law"
	}
	return "µ-law"
}

// Silence returns the encoded byte for a zero sample. The pacer pads with it
// when it has nothing to say, because RTP must keep flowing every 20 ms.
func (l Law) Silence() byte {
	if l == ALaw {
		return 0xD5
	}
	return 0xFF
}

// Encode compands 16-bit PCM samples into G.711 bytes, one byte per sample.
func (l Law) Encode(dst []byte, src []int16) []byte {
	for _, s := range src {
		if l == ALaw {
			dst = append(dst, encodeALaw(s))
		} else {
			dst = append(dst, encodeULaw(s))
		}
	}
	return dst
}

// Decode expands G.711 bytes back into 16-bit PCM samples.
func (l Law) Decode(dst []int16, src []byte) []int16 {
	for _, b := range src {
		if l == ALaw {
			dst = append(dst, decodeALaw(b))
		} else {
			dst = append(dst, decodeULaw(b))
		}
	}
	return dst
}

// The encoders follow the ITU-T G.711 segment tables: the input magnitude is
// reduced to the law's native precision (14 bits for µ-law, 13 for A-law),
// the segment is the position of its leading bit, and the next four bits are
// the step within the segment. Reducing with an arithmetic shift, before
// taking the magnitude, matches the ITU reference implementation bit for bit.

// µ-law segment upper bounds, on the biased 14-bit magnitude.
var ulawSegEnd = [8]int{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}

// A-law segment upper bounds, on the 13-bit magnitude.
var alawSegEnd = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

const (
	ulawBias = 0x21 // 33: shifts the 14-bit magnitude so every segment starts on a power of two
	ulawClip = 8159 // the largest magnitude µ-law can represent, in 14 bits
)

// segment returns the index of the first segment whose upper bound is at least
// v, or len(ends) when v is beyond the last one.
func segment(v int, ends *[8]int) int {
	for i, end := range ends {
		if v <= end {
			return i
		}
	}
	return len(ends)
}

func encodeULaw(s int16) byte {
	v := int(s) >> 2
	mask := byte(0xFF) // µ-law transmits every bit inverted
	if v < 0 {
		v = -v
		mask = 0x7F // and the sign bit set to 0 for negative samples
	}
	v = min(v, ulawClip) + ulawBias

	seg := segment(v, &ulawSegEnd)
	if seg >= len(ulawSegEnd) {
		return 0x7F ^ mask
	}
	return byte(seg<<4|(v>>(seg+1))&0x0F) ^ mask
}

func decodeULaw(b byte) int16 {
	b = ^b
	seg := int(b>>4) & 0x07
	// Rebuild the middle of the step, in 16-bit units (bias 0x84 = 33 << 2).
	t := ((int(b&0x0F) << 3) + 0x84) << seg
	if b&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

func encodeALaw(s int16) byte {
	v := int(s) >> 3
	mask := byte(0xD5) // A-law inverts every even bit, and sets the sign bit for positive samples
	if v < 0 {
		v = -v - 1 // one's complement, so the negative range mirrors the positive one exactly
		mask = 0x55
	}

	seg := segment(v, &alawSegEnd)
	if seg >= len(alawSegEnd) {
		return 0x7F ^ mask
	}
	step := v >> seg
	if seg < 2 {
		step = v >> 1 // the first two segments share the same step size
	}
	return byte(seg<<4|step&0x0F) ^ mask
}

func decodeALaw(b byte) int16 {
	b ^= 0x55
	seg := int(b>>4) & 0x07
	t := int(b&0x0F) << 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t = (t + 0x108) << (seg - 1)
	}
	if b&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}
