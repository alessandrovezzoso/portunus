package media

import (
	"encoding/binary"
	"io"
)

// PCM16Reader reads 16-bit little-endian mono PCM from a byte stream, such as
// a TTS response body, and returns samples.
//
// Network reads end wherever the packets end, so a read can split a sample
// across two calls. The reader carries the odd byte over to the next read
// instead of misaligning everything that follows it.
type PCM16Reader struct {
	r        io.Reader
	buf      []byte
	carry    byte
	hasCarry bool
}

// NewPCM16Reader returns a reader of the samples in r.
func NewPCM16Reader(r io.Reader) *PCM16Reader {
	return &PCM16Reader{r: r}
}

// Read fills dst with up to len(dst) samples and returns how many it read. At
// the end of the stream it returns io.EOF; a trailing half sample is dropped.
func (p *PCM16Reader) Read(dst []int16) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	want := 2 * len(dst)
	if cap(p.buf) < want {
		p.buf = make([]byte, want)
	}
	buf := p.buf[:want]

	start := 0
	if p.hasCarry {
		buf[0] = p.carry
		start = 1
	}
	n, err := p.r.Read(buf[start:])
	n += start

	samples := n / 2
	for i := range samples {
		dst[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	p.hasCarry = n%2 == 1
	if p.hasCarry {
		p.carry = buf[n-1]
	}
	if err == io.EOF && samples > 0 {
		// Hand back the samples now; the next call reports the EOF.
		return samples, nil
	}
	return samples, err
}
