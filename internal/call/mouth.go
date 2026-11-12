package call

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/speech"
)

// mouth is what the assistant says through: fixed lines, the holding phrase,
// and replies spoken while the model is still writing them.
type mouth struct {
	tts   TTS
	law   media.Law
	pacer *media.Pacer
	log   *slog.Logger

	// busy is set while speech is being produced. Together with the audio
	// still queued in the pacer it tells whether the assistant is audible:
	// between two sentences the queue can run dry while the next one is
	// still being synthesized, and that gap is not the caller's turn.
	busy atomic.Bool
}

// audible reports whether the caller is hearing, or about to hear, the
// assistant.
func (m *mouth) audible() bool {
	return m.busy.Load() || m.pacer.Len() > 0
}

// say speaks one fixed line, and returns once its audio is queued.
func (m *mouth) say(ctx context.Context, text string) error {
	m.busy.Store(true)
	defer m.busy.Store(false)
	_, _, err := speak(ctx, m.tts, prepare(text), m.law, m.pacer)
	return err
}

// play queues audio prepared in advance: 8 kHz PCM, such as a holding phrase.
// It plays at once, without a round trip to the TTS.
func (m *mouth) play(pcm []int16) {
	_, _ = m.pacer.Write(m.law.Encode(nil, pcm))
}

// prepare makes one sentence fit for the ear: nothing that only makes sense
// on a screen, and numbers spelled out.
func prepare(text string) string {
	return speech.NormalizeItalian(speech.Sanitize(text))
}

// spokenReply speaks a reply as it streams in. Text goes in through push,
// whole sentences come out of the splitter, and a goroutine synthesizes them
// one after the other, so the first sentence is heard while the model is
// still writing the second.
type spokenReply struct {
	m         *mouth
	split     *speech.Splitter
	sentences chan string
	done      chan struct{}

	// Written by the goroutine, read only after done is closed.
	ttsFirst time.Duration // TTS latency of the first sentence
	onWire   time.Time     // when the first audio of the reply reaches the wire
	err      error
}

// begin starts speaking a reply. finish must be called exactly once.
func (m *mouth) begin(ctx context.Context) *spokenReply {
	m.busy.Store(true)
	r := &spokenReply{
		m:     m,
		split: speech.NewSplitter(),
		// A reply is a few sentences; the buffer only lets the model run a
		// little ahead of the TTS.
		sentences: make(chan string, 16),
		done:      make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		for s := range r.sentences {
			if r.err != nil {
				continue // keep draining, so push never blocks for ever
			}
			first, onWire, err := speak(ctx, m.tts, s, m.law, m.pacer)
			if r.onWire.IsZero() && !onWire.IsZero() {
				r.ttsFirst, r.onWire = first, onWire
			}
			r.err = err
		}
	}()
	return r
}

// push adds streamed text.
func (r *spokenReply) push(text string) {
	for _, s := range r.split.Push(text) {
		r.send(s)
	}
}

// send queues one sentence of the model's reply. Pause tags are stripped
// first: they are instructions to the TTS, and only the fixed texts may give
// them. Otherwise a caller could talk the model into writing "[pause:300s]"
// and leave the line silent, with Kokoro busy producing that silence.
func (r *spokenReply) send(sentence string) {
	if p := prepare(speech.StripPauses(sentence)); p != "" {
		r.sentences <- p
	}
}

// finish speaks what is left and waits until all of the reply is queued for
// the wire. It returns the TTS latency of the first sentence and when the
// first audio reaches the wire (zero if nothing was spoken).
func (r *spokenReply) finish() (ttsFirst time.Duration, onWire time.Time, err error) {
	if rest := r.split.Flush(); rest != "" {
		r.send(rest)
	}
	close(r.sentences)
	<-r.done
	r.m.busy.Store(false)
	return r.ttsFirst, r.onWire, r.err
}
