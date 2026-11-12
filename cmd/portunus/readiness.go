package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// readiness decides, on every INVITE of the agent mode, whether Portunus can
// serve the call. If it cannot, the call is refused with 486 and the S20
// sends it to voicemail, which beats answering with an assistant that cannot
// hear or speak.
//
// Two things are checked:
//   - The speech servers answer a quick ping. They run on this host, so
//     anything slower than pingTimeout means they are down or stuck.
//   - The model account works. That is known only from a failed call (no
//     credit, a revoked key): the agent reports it through markDown. While
//     it is down, a one-token probe runs at most once a minute, in the
//     background, and its first success opens the door again.
type readiness struct {
	tts   pinger
	stt   pinger
	probe func(context.Context) error
	log   *slog.Logger

	mu        sync.Mutex
	llmDown   error
	lastProbe time.Time
	probing   bool
}

type pinger interface{ Ping(context.Context) error }

const (
	pingTimeout = 300 * time.Millisecond
	probeEvery  = time.Minute
	// probeTimeout bounds one probe. It runs in the background, not while a
	// caller waits for the phone to ring, so it can afford a slow network.
	probeTimeout = 15 * time.Second
)

// markDown records that the model cannot serve calls.
func (r *readiness) markDown(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.llmDown = err
	r.lastProbe = time.Now()
}

// ready is sip.Options.Ready.
func (r *readiness) ready(ctx context.Context) error {
	if err := r.model(); err != nil {
		return err
	}

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	ping := func(name string, p pinger) <-chan error {
		ch := make(chan error, 1)
		go func() {
			if err := p.Ping(pctx); err != nil {
				ch <- fmt.Errorf("%s not answering: %w", name, err)
				return
			}
			ch <- nil
		}()
		return ch
	}
	tts, stt := ping("TTS", r.tts), ping("STT", r.stt)
	return errors.Join(<-tts, <-stt)
}

// model reports whether the model account is usable. When it is known to be
// down and a probe is due, it starts one in the background.
//
// The probe used to run here, inside the INVITE's one-second budget: a real
// request to the API often takes longer than that, and a timed-out probe kept
// the door shut for another minute. The price of running it apart is that the
// call which starts it is still refused; the next one gets through.
func (r *readiness) model() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.llmDown == nil {
		return nil
	}
	if !r.probing && time.Since(r.lastProbe) >= probeEvery {
		r.probing = true
		r.lastProbe = time.Now()
		go r.runProbe()
	}
	return fmt.Errorf("model unavailable: %w", r.llmDown)
}

// runProbe sends one probe and records the outcome.
func (r *readiness) runProbe() {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	err := r.probe(ctx)

	r.mu.Lock()
	r.probing = false
	if err == nil {
		r.llmDown = nil
	}
	r.mu.Unlock()

	if err != nil {
		r.log.Warn("model still unavailable; calls go to voicemail", "error", err)
		return
	}
	r.log.Info("model available again: answering calls")
}

// ttsPinger turns the TTS adapter's Check, a cheap GET, into a ping.
type ttsPinger struct{ check func(context.Context) error }

func (t ttsPinger) Ping(ctx context.Context) error { return t.check(ctx) }
