package call

import "context"

// STT turns one finished utterance into text.
//
// It works on whole utterances because Whisper does: it transcribes a phrase
// only once the phrase is over. A streaming STT, which returns words while the
// caller is still speaking, would need a different interface; it will get its
// own when one is added.
type STT interface {
	// SampleRate is the rate, in Hz, of the PCM that Transcribe expects.
	SampleRate() int
	// Transcribe returns the text spoken in pcm, 16-bit mono at SampleRate.
	// Cancelling ctx must abort the request.
	Transcribe(ctx context.Context, pcm []int16) (string, error)
}

// VAD tells speech from everything else, one short frame at a time.
//
// A VAD keeps state between frames (Silero runs an LSTM), so each call needs
// its own: Listen takes a function that makes one.
type VAD interface {
	// SampleRate is the rate, in Hz, of the frames Process expects.
	SampleRate() int
	// FrameSize is the number of samples Process takes per call.
	FrameSize() int
	// Process returns the probability, from 0 to 1, that frame holds speech.
	// Frames must arrive in order, FrameSize samples each.
	Process(frame []int16) float32
}
