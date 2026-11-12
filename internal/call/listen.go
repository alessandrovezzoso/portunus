package call

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// ListenOptions configures the listen mode. Zero values mean the defaults.
type ListenOptions struct {
	// VADThreshold is the speech probability at which an utterance starts.
	// Raising it is the fix for a handset that echoes the assistant back.
	VADThreshold float32
	// VADSilence is how long the caller must stay silent for an utterance to
	// end. Every turn waits this long before the STT starts.
	VADSilence time.Duration
	// MaxUtterance cuts a longer monologue into pieces. It must fit the STT's
	// input window: 30 s for Whisper, less if its audio context is reduced.
	MaxUtterance time.Duration

	Logger *slog.Logger

	// drain overrides drainTimeout, for tests.
	drain time.Duration
}

const (
	// queuedUtterances bounds the utterances waiting for the STT. The inbound
	// loop must never wait for the STT, so when the STT falls this far behind,
	// new utterances are dropped and logged instead.
	queuedUtterances = 4

	// drainTimeout bounds how long a call that has ended keeps transcribing
	// what it already captured. Callers often hang up right after their last
	// phrase, and that phrase ("call me back at…") is the one that matters,
	// so it is not thrown away; but a stuck STT must not hold a call slot, or
	// a shutdown, for ever.
	drainTimeout = 20 * time.Second

	// requestTimeout bounds one transcription. Whisper on a CPU can take
	// several seconds; anything past this is a server that is stuck.
	requestTimeout = 30 * time.Second
)

// Listen is the handler for the listen mode: it answers, stays silent, and
// transcribes the caller. It proves the inbound half of the pipeline, RTP →
// G.711 → resample → VAD → STT, on a real phone.
//
// Transcripts are logged only at debug level: they are what a third party
// said. At info level each utterance logs only its length and timing.
func Listen(stt STT, newVAD func() (VAD, error), opts ListenOptions) sip.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	segCfg := segmenterConfigFor(opts.VADThreshold, opts.VADSilence, opts.MaxUtterance)
	drain := opts.drain
	if drain <= 0 {
		drain = drainTimeout
	}

	return func(ctx context.Context, c *sip.Call) error {
		log := log.With("call_id", c.ID)
		vad, err := newVAD()
		if err != nil {
			return fmt.Errorf("vad: %w", err)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// The assistant says nothing, but RTP must still flow out: a PBX may
		// drop a call whose far end sends no media. The pacer sends encoded
		// silence when it has nothing queued, which here is always.
		law := lawFor(c.Codec)
		pacer := media.NewPacer(law.Silence(), media.BytesPerSecond)
		paced := make(chan error, 1)
		go func() { paced <- pacer.Run(ctx, c.Outbound) }()

		// Transcription runs in its own goroutine, so a slow STT delays only
		// the transcripts, never the reading of inbound audio. Its context is
		// detached from the call's, which ends at the hangup: what was already
		// captured still gets transcribed, within drain.
		e := newEar(vad, law, segCfg, nil, log)
		t := &transcriber{stt: stt, vadRate: vad.SampleRate(), log: log}
		sttCtx, stopSTT := context.WithCancel(context.WithoutCancel(ctx))
		defer stopSTT()
		transcribed := make(chan struct{})
		go func() {
			t.run(sttCtx, e.queue)
			close(transcribed)
		}()

		// The ear closes its queue when the call's audio ends, which lets the
		// transcriber finish.
		readErr := e.run(ctx, c.Inbound)

		// The call is over: stop the silence, then let the transcriber finish
		// the queue, for drain at most.
		cancel()
		runErr := <-paced
		select {
		case <-transcribed:
		case <-time.After(drain):
			stopSTT()
			<-transcribed
		}

		t.logSummary(int(e.dropped.Load()))
		if readErr != nil {
			return readErr
		}
		// A hangup closes the media under the pacer, possibly before ctx is
		// cancelled: that write error is the call ending, not a failure.
		if runErr != nil && !mediaClosed(runErr) {
			return fmt.Errorf("send silence: %w", runErr)
		}
		return nil
	}
}

// transcriber sends queued utterances to the STT and keeps the latency
// numbers for the end-of-call summary. Only its own goroutine touches them
// until run returns.
type transcriber struct {
	stt     STT
	vadRate int
	log     *slog.Logger

	turns     int
	sttTotal  time.Duration
	sttMax    time.Duration
	failed    int
	abandoned int
}

func (t *transcriber) run(ctx context.Context, queue <-chan queued) {
	for q := range queue {
		if ctx.Err() != nil {
			t.abandoned++ // past the drain deadline: empty the queue unsent
			continue
		}
		pcm := q.pcm
		if rate := t.stt.SampleRate(); rate != t.vadRate {
			pcm = resampleAll(pcm, t.vadRate, rate)
		}

		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		text, err := t.stt.Transcribe(rctx, pcm)
		cancel()
		elapsed := time.Since(q.ended)
		if err != nil {
			if ctx.Err() != nil {
				t.abandoned++ // cut off by the drain deadline, not a failure
				continue
			}
			t.failed++
			t.log.Warn("transcription failed", "turn", q.turn, "speech_ms", q.speech.Milliseconds(), "error", err)
			continue
		}

		t.turns++
		t.sttTotal += elapsed
		t.sttMax = max(t.sttMax, elapsed)
		attrs := []any{
			"turn", q.turn,
			"speech_ms", q.speech.Milliseconds(),
			"stt_ms", elapsed.Milliseconds(),
			"chars", len([]rune(text)),
		}
		if q.cut {
			attrs = append(attrs, "cut", true)
		}
		t.log.Info("utterance", attrs...)
		// The words a third party said: debug level only, never at info.
		t.log.Debug("transcript", "turn", q.turn, "text", text)
	}
}

// logSummary writes the end-of-call latency line.
func (t *transcriber) logSummary(dropped int) {
	attrs := []any{"turns", t.turns}
	if t.turns > 0 {
		attrs = append(attrs,
			"stt_ms_avg", (t.sttTotal / time.Duration(t.turns)).Milliseconds(),
			"stt_ms_max", t.sttMax.Milliseconds(),
		)
	}
	if t.failed > 0 {
		attrs = append(attrs, "stt_failed", t.failed)
	}
	if t.abandoned > 0 {
		attrs = append(attrs, "abandoned_utterances", t.abandoned)
	}
	if dropped > 0 {
		attrs = append(attrs, "dropped_utterances", dropped)
	}
	t.log.Info("call latency", attrs...)
}

// resampleAll converts one whole utterance between rates.
func resampleAll(pcm []int16, from, to int) []int16 {
	rs := media.NewResampler(from, to)
	out := rs.Process(nil, pcm)
	return rs.Flush(out)
}

// inboundEnded reports whether a read error just means the call has ended:
// the context was cancelled, or a hangup on either side closed the media.
func inboundEnded(ctx context.Context, err error) bool {
	return ctx.Err() != nil || mediaClosed(err)
}

// mediaClosed reports whether err is the media being closed by a hangup.
func mediaClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// segmenterConfigFor fills in the defaults for the settings a mode exposes.
func segmenterConfigFor(threshold float32, silence, maxUtterance time.Duration) segmenterConfig {
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	if silence <= 0 {
		silence = defaultSilence
	}
	if maxUtterance <= 0 {
		maxUtterance = defaultMaxUtterance
	}
	return segmenterConfig{
		threshold:    threshold,
		silence:      silence,
		minSpeech:    defaultMinSpeech,
		preRoll:      defaultPreRoll,
		maxUtterance: maxUtterance,
	}
}
