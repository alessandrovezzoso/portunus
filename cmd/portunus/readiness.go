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
//     it is down, a one-token probe runs at most once a minute, and the first
//     success opens the door again.
type readiness struct {
	tts   pinger
	stt   pinger
	probe func(context.Context) error
	log   *slog.Logger

	mu        sync.Mutex
	llmDown   error
	lastProbe time.Time
}

type pinger interface{ Ping(context.Context) error }

const (
	pingTimeout = 300 * time.Millisecond
	probeEvery  = time.Minute
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
	if err := r.model(ctx); err != nil {
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

// model reports whether the model account is usable, probing it when it is
// known to be down and a probe is due.
func (r *readiness) model(ctx context.Context) error {
	r.mu.Lock()
	down, due := r.llmDown, time.Since(r.lastProbe) >= probeEvery
	if down != nil && due {
		r.lastProbe = time.Now()
	}
	r.mu.Unlock()

	switch {
	case down == nil:
		return nil
	case !due:
		return fmt.Errorf("model unavailable: %w", down)
	}
	if err := r.probe(ctx); err != nil {
		return fmt.Errorf("model still unavailable: %w", err)
	}
	r.mu.Lock()
	r.llmDown = nil
	r.mu.Unlock()
	r.log.Info("model available again: answering calls")
	return nil
}

// ttsPinger turns the TTS adapter's Check, a cheap GET, into a ping.
type ttsPinger struct{ check func(context.Context) error }

func (t ttsPinger) Ping(ctx context.Context) error { return t.check(ctx) }
