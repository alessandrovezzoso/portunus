package call

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// GreetOptions configures the greet mode.
type GreetOptions struct {
	// OutputBuffer caps the audio queued ahead of the RTP clock.
	OutputBuffer time.Duration
	// Tail is how long to wait after the last word before hanging up, so the
	// end of the greeting is not cut by the phone's jitter buffer. Zero
	// means the default of 500 ms.
	Tail time.Duration

	Logger *slog.Logger
}

const defaultTail = 500 * time.Millisecond

// Greet is the handler for the greet mode: it speaks text to the caller and
// hangs up. It proves the outbound half of the pipeline, TTS → resample →
// G.711 → pacer, end to end, on a real phone.
func Greet(tts TTS, text string, opts GreetOptions) sip.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	tail := opts.Tail
	if tail <= 0 {
		tail = defaultTail
	}

	return func(ctx context.Context, c *sip.Call) error {
		answered := time.Now()
		log := log.With("call_id", c.ID)
		law := lawFor(c.Codec)
		pacer := media.NewPacer(law.Silence(), int(opts.OutputBuffer.Seconds()*media.BytesPerSecond))

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Outbound starts at once: RTP must flow while the TTS is still
		// thinking, or the far end may decide the line is dead.
		paced := make(chan error, 1)
		go func() { paced <- pacer.Run(ctx, c.Outbound) }()

		// Nothing is listened to yet, but inbound must still be read: that is
		// how a hangup shows up (io.EOF), and it keeps the socket drained.
		// This goroutine ends with the call, after this handler returns.
		go func() {
			_, _ = io.Copy(io.Discard, c.Inbound)
			cancel()
		}()

		ttsFirst, err := speak(ctx, tts, text, law, pacer)
		if err == nil {
			waitDrained(ctx, pacer)
			sleepCtx(ctx, tail)
		}
		hungUp := ctx.Err() != nil
		cancel()
		runErr := <-paced

		logCallStats(log, pacer.Stats(), answered, ttsFirst)

		switch {
		case hungUp:
			// The caller hung up, or Portunus is shutting down. Either way the
			// greeting simply stops; that is not an error.
			return nil
		case err != nil:
			return fmt.Errorf("greeting: %w", err)
		}
		return runErr
	}
}

// speak streams one utterance from the TTS into the pacer and returns how
// long the first audio took to arrive.
func speak(ctx context.Context, tts TTS, text string, law media.Law, p *media.Pacer) (time.Duration, error) {
	start := time.Now()
	body, err := tts.Synthesize(ctx, text)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	enc := media.NewEncoder(tts.SampleRate(), law)
	pcm := media.NewPCM16Reader(body)
	buf := make([]int16, tts.SampleRate()/10) // 100 ms per read

	// The utterance starts with its first audio, not with the request: the
	// silence before it is TTS latency, measured separately, not starvation.
	var first time.Duration
	defer p.SetSpeaking(false)
	for {
		n, err := pcm.Read(buf)
		if n > 0 {
			if first == 0 {
				first = time.Since(start)
				p.SetSpeaking(true)
			}
			_, _ = p.Write(enc.Encode(buf[:n]))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return first, err
		}
	}
	_, _ = p.Write(enc.Flush())
	return first, nil
}

// waitDrained returns once the pacer has sent everything queued, or ctx ends.
func waitDrained(ctx context.Context, p *media.Pacer) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for p.Len() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// logCallStats writes the two end-of-call diagnostic lines that CLAUDE.md
// asks for, with the fields this mode can measure.
func logCallStats(log *slog.Logger, st media.PacerStats, answered time.Time, ttsFirst time.Duration) {
	log.Info("call audio quality",
		"starved_pct", math.Round(st.StarvedPct()*10)/10,
		"dropped_bytes", st.DroppedBytes,
		"audio_frames", st.AudioFrames,
	)
	attrs := []any{}
	if ttsFirst > 0 {
		attrs = append(attrs, "tts_first_audio_ms", ttsFirst.Milliseconds())
	}
	if !st.FirstAudio.IsZero() {
		attrs = append(attrs, "first_audio_ms", st.FirstAudio.Sub(answered).Milliseconds())
	}
	log.Info("call latency", attrs...)
}

// lawFor maps the codec negotiated on the SIP leg to its G.711 law.
func lawFor(c sip.Codec) media.Law {
	if c == sip.CodecPCMA {
		return media.ALaw
	}
	return media.ULaw
}
