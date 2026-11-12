package media

import "encoding/binary"

// wavHeaderSize is the size of the canonical RIFF/WAVE header: a RIFF chunk
// holding one "fmt " chunk and one "data" chunk.
const wavHeaderSize = 44

// EncodeWAV wraps mono 16-bit PCM at rate Hz in a WAV file, the container STT
// servers accept for an uploaded utterance.
//
// It writes the canonical 44-byte header and nothing else: no LIST or fact
// chunks, which some decoders choke on and none of them needs.
func EncodeWAV(pcm []int16, rate int) []byte {
	const (
		channels      = 1
		bitsPerSample = 16
		blockAlign    = channels * bitsPerSample / 8
	)
	dataSize := len(pcm) * blockAlign
	b := make([]byte, wavHeaderSize+dataSize)
	le := binary.LittleEndian

	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(wavHeaderSize-8+dataSize)) // everything after this field
	copy(b[8:], "WAVE")

	copy(b[12:], "fmt ")
	le.PutUint32(b[16:], 16) // size of the fmt chunk for plain PCM
	le.PutUint16(b[20:], 1)  // format 1: integer PCM
	le.PutUint16(b[22:], channels)
	le.PutUint32(b[24:], uint32(rate))
	le.PutUint32(b[28:], uint32(rate*blockAlign)) // bytes per second
	le.PutUint16(b[32:], blockAlign)
	le.PutUint16(b[34:], bitsPerSample)

	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(dataSize))
	for i, s := range pcm {
		le.PutUint16(b[wavHeaderSize+2*i:], uint16(s))
	}
	return b
}
