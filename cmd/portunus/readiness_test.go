package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func newReadiness(tts, stt error, probe func(context.Context) error) *readiness {
	return &readiness{
		tts: fakePinger{tts}, stt: fakePinger{stt}, probe: probe,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestReadyWhenEverythingAnswers(t *testing.T) {
	r := newReadiness(nil, nil, nil)
	if err := r.ready(context.Background()); err != nil {
		t.Errorf("ready = %v, want nil", err)
	}
}

func TestNotReadyWhenASpeechServerIsDown(t *testing.T) {
	for name, r := range map[string]*readiness{
		"tts": newReadiness(errors.New("connection refused"), nil, nil),
		"stt": newReadiness(nil, errors.New("connection refused"), nil),
	} {
		if err := r.ready(context.Background()); err == nil {
			t.Errorf("%s down: ready = nil, want an error", name)
		}
	}
}

// scriptedProbe answers probes with err and counts them.
type scriptedProbe struct {
	mu    sync.Mutex
	err   error
	count int
}

func (p *scriptedProbe) probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	return p.err
}

func (p *scriptedProbe) probes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// settle waits until no probe is running, so the test reads its outcome.
func settle(t *testing.T, r *readiness) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		probing := r.probing
		r.mu.Unlock()
		if !probing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestModelDownRefusesUntilAProbeSucceeds(t *testing.T) {
	p := &scriptedProbe{err: errors.New("still no credit")}
	r := newReadiness(nil, nil, p.probe)

	r.markDown(errors.New("billing_error"))
	if err := r.ready(context.Background()); err == nil {
		t.Fatal("a model marked down must refuse calls")
	}
	settle(t, r)
	if n := p.probes(); n != 0 {
		t.Errorf("probed %d times right after the failure, want 0: probes are rate limited", n)
	}

	// A minute later a probe is due. The call that starts it is refused
	// either way: the probe runs in the background. It fails, so calls stay
	// refused.
	r.lastProbe = time.Now().Add(-probeEvery)
	if err := r.ready(context.Background()); err == nil {
		t.Error("the call that starts a probe must still be refused")
	}
	settle(t, r)
	if err := r.ready(context.Background()); err == nil || p.probes() != 1 {
		t.Errorf("ready = %v after %d probes, want refused after 1", err, p.probes())
	}

	// The next due probe succeeds, and the door opens for the next call.
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	r.lastProbe = time.Now().Add(-probeEvery)
	_ = r.ready(context.Background())
	settle(t, r)
	if err := r.ready(context.Background()); err != nil {
		t.Errorf("ready = %v after a good probe, want nil", err)
	}
	if err := r.ready(context.Background()); err != nil || p.probes() != 2 {
		t.Errorf("once up, no more probes: ready = %v, probes = %d", err, p.probes())
	}
}

func TestOnlyOneProbeRunsAtATime(t *testing.T) {
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)
	calls := 0
	var mu sync.Mutex
	r := newReadiness(nil, nil, func(context.Context) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			started.Done()
		}
		<-release
		return nil
	})
	r.markDown(errors.New("billing_error"))
	r.lastProbe = time.Now().Add(-probeEvery)

	_ = r.ready(context.Background())
	started.Wait()
	// The probe is still running, and a minute has notionally passed again:
	// a second INVITE must not start another one.
	r.mu.Lock()
	r.lastProbe = time.Now().Add(-probeEvery)
	r.mu.Unlock()
	_ = r.ready(context.Background())
	close(release)
	settle(t, r)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("%d probes ran at once, want 1", calls)
	}
}
