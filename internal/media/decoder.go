package media

// Decoder turns G.711 bytes from the SIP leg into PCM at the rate an adapter
// declares (16 kHz for the VAD and the STT). It is the inbound mirror of
// Encoder, and the one place where inbound audio changes format and rate.
type Decoder struct {
	law Law
	rs  *Resampler // nil when the output stays at 8 kHz
	pcm []int16    // reused: the decoded 8 kHz samples
	out []int16    // reused: the resampled samples
}

// NewDecoder returns a decoder from law to 16-bit PCM at outRate Hz.
func NewDecoder(law Law, outRate int) *Decoder {
	d := &Decoder{law: law}
	if outRate != TelephoneRate {
		d.rs = NewResampler(TelephoneRate, outRate)
	}
	return d
}

// Decode converts the next RTP payload. The returned slice is reused by the
// next call, so the caller must copy what it keeps: inbound audio arrives 50
// times a second for the whole call, and allocating each frame would only
// feed the garbage collector.
func (d *Decoder) Decode(payload []byte) []int16 {
	d.pcm = d.law.Decode(d.pcm[:0], payload)
	if d.rs == nil {
		return d.pcm
	}
	d.out = d.rs.Process(d.out[:0], d.pcm)
	return d.out
}
