package media

import (
	"context"
	"io"
	"sync"
	"time"
)

// FrameSize is one RTP frame: 20 ms of G.711 at 8 kHz, one byte per sample.
const FrameSize = 160

// BytesPerSecond is the G.711 data rate, used to size the pacer's buffer.
const BytesPerSecond = TelephoneRate

// Pacer sits between bursty audio and the steady RTP clock.
//
// A TTS delivers speech in bursts, often faster than real time, while RTP
// needs exactly one 160-byte frame every 20 ms. Writers queue encoded audio
// with Write; Run sends one frame per tick and pads with encoded silence when
// the queue is empty, so the far end never sees a gap.
//
// The 20 ms clock is not here: it is the writer passed to Run. diago's RTP
// writer blocks every Write until its next 20 ms tick, and that clock cannot
// be turned off, so a second ticker here would only fight it. Writing "whenever
// audio arrives" instead of once per tick is what makes audio choppy.
type Pacer struct {
	mu       sync.Mutex
	queue    []byte
	max      int
	silence  byte
	speaking bool
	stats    PacerStats
}

// PacerStats describes what a pacer sent. They feed the end-of-call
// "call audio quality" log line.
type PacerStats struct {
	AudioFrames   int64 // frames that carried queued audio
	PaddedFrames  int64 // frames of pure silence
	StarvedFrames int64 // silence sent while an utterance was in progress
	DroppedBytes  int64 // audio discarded because the queue was full
	FlushedBytes  int64 // audio discarded by Flush (barge-in)

	FirstAudio time.Time // when the first frame of real audio was sent
}

// StarvedPct is the share of an utterance's frames that had to be padded with
// silence because the pipeline could not keep up with the RTP clock.
func (s PacerStats) StarvedPct() float64 {
	total := s.AudioFrames + s.StarvedFrames
	if total == 0 {
		return 0
	}
	return 100 * float64(s.StarvedFrames) / float64(total)
}

// NewPacer returns a pacer that pads with silence and queues at most
// maxBytes. The cap must hold a whole reply, or speech is cut mid-utterance.
func NewPacer(silence byte, maxBytes int) *Pacer {
	return &Pacer{silence: silence, max: max(maxBytes, FrameSize)}
}

// Write queues encoded audio. When the queue would exceed its cap, the oldest
// bytes are dropped: the newest audio is what the caller is about to hear.
func (p *Pacer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, b...)
	if over := len(p.queue) - p.max; over > 0 {
		p.queue = append(p.queue[:0], p.queue[over:]...)
		p.stats.DroppedBytes += int64(over)
	}
	return len(b), nil
}

// Len reports how many bytes are queued, which is audio the caller has not
// heard yet.
func (p *Pacer) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Flush discards everything queued. On barge-in, this is what silences the
// assistant, and it returns the number of bytes discarded.
func (p *Pacer) Flush() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.queue)
	p.queue = p.queue[:0]
	p.stats.FlushedBytes += int64(n)
	return n
}

// SetSpeaking marks whether an utterance is in progress. Padding while
// speaking counts as starvation: the RTP clock outran the pipeline.
func (p *Pacer) SetSpeaking(speaking bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.speaking = speaking
}

// Stats returns a snapshot of what has been sent so far.
func (p *Pacer) Stats() PacerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Run sends one frame per Write to w until ctx is cancelled or w fails. w must
// pace itself, one frame per 20 ms, as diago's RTP writer does.
func (p *Pacer) Run(ctx context.Context, w io.Writer) error {
	frame := make([]byte, FrameSize)
	for ctx.Err() == nil {
		p.next(frame)
		if _, err := w.Write(frame); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}

// next fills frame from the queue, padding a short tail or an empty queue with
// silence, and updates the statistics.
func (p *Pacer) next(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := copy(frame, p.queue)
	for i := n; i < len(frame); i++ {
		frame[i] = p.silence
	}
	p.queue = append(p.queue[:0], p.queue[n:]...)

	switch {
	case n > 0:
		p.stats.AudioFrames++
		if p.stats.FirstAudio.IsZero() {
			p.stats.FirstAudio = time.Now()
		}
	case p.speaking:
		p.stats.StarvedFrames++
		p.stats.PaddedFrames++
	default:
		p.stats.PaddedFrames++
	}
}
