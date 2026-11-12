package call

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// ear turns the caller's audio into utterances: RTP payloads are decoded,
// cut into VAD frames, and the segmenter decides where each phrase starts and
// ends. Finished utterances go to queue, which the ear closes when the call's
// audio ends.
//
// The ear never waits for anyone: when the queue is full, an utterance is
// dropped and counted, because inbound RTP must be read as it arrives.
type ear struct {
	vad   VAD
	seg   *segmenter
	dec   *media.Decoder
	queue chan queued
	log   *slog.Logger

	// muted, when set, reports that the assistant is talking. Until barge-in
	// exists, speech heard then is not the caller's turn: it is counted and
	// thrown away. That also keeps the handset's echo of the assistant out
	// of the conversation.
	muted func() bool

	dropped     atomic.Int64 // utterances lost to a full queue
	ignored     atomic.Int64 // speech heard while muted
	mutedSpeech bool         // the ear is inside a stretch of muted speech
	turn        int
}

// queued is an utterance on its way to the STT, stamped with the moment the
// VAD ended it, which is where stt_ms starts.
type queued struct {
	utterance
	ended time.Time
	turn  int
}

func newEar(vad VAD, law media.Law, cfg segmenterConfig, muted func() bool, log *slog.Logger) *ear {
	cfg.rate = vad.SampleRate()
	return &ear{
		vad:   vad,
		seg:   newSegmenter(cfg),
		dec:   media.NewDecoder(law, vad.SampleRate()),
		queue: make(chan queued, queuedUtterances),
		log:   log,
		muted: muted,
	}
}

// run reads inbound audio until the call ends. It returns an error only for a
// failure that is not the call ending.
func (e *ear) run(ctx context.Context, in io.Reader) error {
	defer close(e.queue)
	buf := make([]byte, sip.InboundBufSize)
	frameSize := e.vad.FrameSize()
	var pending []int16 // decoded audio not yet a full VAD frame

	for {
		n, err := in.Read(buf)
		if err != nil {
			if inboundEnded(ctx, err) {
				return nil
			}
			return fmt.Errorf("read inbound audio: %w", err)
		}
		if n == 0 {
			continue // diago reports keepalives and empty payloads as zero-length reads
		}

		// RTP frames (20 ms) and VAD frames (32 ms for Silero) do not line
		// up, so decoded audio collects in pending until a VAD frame is full.
		pending = append(pending, e.dec.Decode(buf[:n])...)
		for len(pending) >= frameSize {
			e.frame(pending[:frameSize])
			pending = pending[:copy(pending, pending[frameSize:])]
		}
	}
}

// frame handles one VAD frame.
func (e *ear) frame(frame []int16) {
	// The VAD sees every frame, muted or not: it keeps state across frames
	// and must not lose track of the audio.
	prob := e.vad.Process(frame)

	if e.muted != nil && e.muted() {
		switch {
		case prob >= e.seg.cfg.threshold && !e.mutedSpeech:
			e.mutedSpeech = true
			e.ignored.Add(1)
		case prob < e.seg.off:
			e.mutedSpeech = false
		}
		e.seg.reset()
		return
	}
	e.mutedSpeech = false

	u := e.seg.push(frame, prob)
	if u == nil {
		return
	}
	e.turn++
	select {
	case e.queue <- queued{utterance: *u, ended: time.Now(), turn: e.turn}:
	default:
		e.dropped.Add(1)
		e.log.Warn("utterance dropped: the STT is too far behind",
			"turn", e.turn, "speech_ms", u.speech.Milliseconds(), "queued", queuedUtterances)
	}
}
