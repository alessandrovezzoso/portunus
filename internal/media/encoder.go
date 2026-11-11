package media

// TelephoneRate is the sample rate of the SIP leg: G.711 at 8 kHz.
const TelephoneRate = 8000

// Encoder turns PCM at an adapter's sample rate into G.711 bytes for the SIP
// leg. It is the one place where outbound audio changes rate and format.
type Encoder struct {
	law Law
	rs  *Resampler // nil when the input is already at 8 kHz
	pcm []int16    // reused output buffer of the resampler
}

// NewEncoder returns an encoder from 16-bit PCM at inRate Hz to law.
func NewEncoder(inRate int, law Law) *Encoder {
	e := &Encoder{law: law}
	if inRate != TelephoneRate {
		e.rs = NewResampler(inRate, TelephoneRate)
	}
	return e
}

// Encode converts the next piece of PCM and returns the G.711 bytes. The
// returned slice is freshly allocated, so it can be queued as is.
func (e *Encoder) Encode(pcm []int16) []byte {
	if e.rs == nil {
		return e.law.Encode(make([]byte, 0, len(pcm)), pcm)
	}
	e.pcm = e.rs.Process(e.pcm[:0], pcm)
	return e.law.Encode(make([]byte, 0, len(e.pcm)), e.pcm)
}

// Flush returns the audio still held by the resampler. Call it once, after
// the last Encode of a stream.
func (e *Encoder) Flush() []byte {
	if e.rs == nil {
		return nil
	}
	e.pcm = e.rs.Flush(e.pcm[:0])
	return e.law.Encode(make([]byte, 0, len(e.pcm)), e.pcm)
}
