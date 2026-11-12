// Package silero is a VAD adapter for the Silero VAD model, run in process by
// github.com/zserge/govad: a pure Go implementation with the model weights
// embedded, so it needs no cgo, no ONNX runtime and no separate service.
//
// Silero is a small neural network (an LSTM over short spectra) trained to
// tell human speech from noise. Unlike a loudness threshold, it is not fooled
// by line noise or comfort noise, which matters on the phone.
package silero

import (
	"fmt"

	"github.com/zserge/govad"

	"github.com/alessandrovezzoso/portunus/internal/media"
)

// SampleRate is the only rate govad's weights accept.
const SampleRate = 16000

// VAD runs Silero on one audio stream. It keeps the network's state between
// frames, so each call needs its own.
type VAD struct {
	model *govad.VAD
	buf   []float32 // reused: the frame converted for the model
}

// New returns a detector with fresh state. Loading the embedded weights takes
// a few milliseconds, which is fine once per call.
func New() (*VAD, error) {
	m, err := govad.New()
	if err != nil {
		return nil, fmt.Errorf("silero: %w", err)
	}
	return &VAD{model: m, buf: make([]float32, 0, govad.SamplesPerFrame)}, nil
}

// SampleRate reports the rate Process expects.
func (v *VAD) SampleRate() int { return SampleRate }

// FrameSize reports the samples per Process call: 512, or 32 ms.
func (v *VAD) FrameSize() int { return govad.SamplesPerFrame }

// Process returns the probability that frame holds speech. A frame of the
// wrong size is a programming error, and panics.
func (v *VAD) Process(frame []int16) float32 {
	if len(frame) != govad.SamplesPerFrame {
		panic(fmt.Sprintf("silero: frame of %d samples, want %d", len(frame), govad.SamplesPerFrame))
	}
	v.buf = media.ToFloat32(v.buf[:0], frame)
	return v.model.Process(v.buf)
}
