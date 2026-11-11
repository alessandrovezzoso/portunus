// Package call runs the session of an answered call: what Portunus says and
// hears. It depends only on interfaces for the speech engines, so adapters
// can be swapped and tests can script whole calls with fakes.
//
// Adapters never import this package. The interfaces live here, with the code
// that uses them, and they use only standard library types.
package call

import (
	"context"
	"io"
)

// TTS turns text into speech.
type TTS interface {
	// SampleRate is the rate, in Hz, of the PCM that Synthesize returns.
	SampleRate() int
	// Synthesize streams speech for text as 16-bit little-endian mono PCM.
	// Audio should start arriving before the whole text is synthesized. The
	// caller closes the body; cancelling ctx must abort the stream.
	Synthesize(ctx context.Context, text string) (io.ReadCloser, error)
}
