package media

// ToFloat32 appends src to dst as float32 samples in [-1, 1), the format
// neural models such as Silero VAD take, and returns the extended slice.
//
// The scale is 1/32768, not 1/32767, so -32768 maps exactly to -1 and the
// conversion is the same plain division everywhere. +32767 lands just below
// 1, which no model cares about.
func ToFloat32(dst []float32, src []int16) []float32 {
	for _, s := range src {
		dst = append(dst, float32(s)/32768)
	}
	return dst
}
