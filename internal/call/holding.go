package call

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/alessandrovezzoso/portunus/internal/media"
)

// DefaultHoldingPhrases are said while the reply is being prepared, when no
// file overrides them. They are a little longer than "Un attimo." on purpose:
// Kokoro sounds wrong on very short utterances.
var DefaultHoldingPhrases = []string{
	"Certo, un momento.",
	"Bene, un attimo per favore.",
	"Va bene, mi dia un momento.",
}

// PrepareHolding synthesizes the holding phrases once, at startup, so a call
// can play one the instant the caller stops talking. They are kept at 8 kHz
// and encoded per call, because the G.711 law is known only when a call is
// answered.
func PrepareHolding(ctx context.Context, tts TTS, phrases []string) ([][]int16, error) {
	clips := make([][]int16, 0, len(phrases))
	for _, p := range phrases {
		pcm, err := synthesizeAll(ctx, tts, prepare(p))
		if err != nil {
			return nil, fmt.Errorf("holding phrase %q: %w", p, err)
		}
		// Kokoro leads with a little silence; a holding phrase is only useful
		// if it starts right away.
		clips = append(clips, media.TrimSilence(pcm, media.TelephoneRate))
	}
	return clips, nil
}

// synthesizeAll reads a whole utterance from the TTS, at 8 kHz.
func synthesizeAll(ctx context.Context, tts TTS, text string) ([]int16, error) {
	body, err := tts.Synthesize(ctx, text)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	rs := media.NewResampler(tts.SampleRate(), media.TelephoneRate)
	r := media.NewPCM16Reader(body)
	buf := make([]int16, tts.SampleRate()/10)
	var out []int16
	for {
		n, err := r.Read(buf)
		out = rs.Process(out, buf[:n])
		if errors.Is(err, io.EOF) {
			return rs.Flush(out), nil
		}
		if err != nil {
			return nil, err
		}
	}
}
