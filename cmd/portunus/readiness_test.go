package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func TestModelDownRefusesUntilAProbeSucceeds(t *testing.T) {
	probes := 0
	probeErr := errors.New("still no credit")
	r := newReadiness(nil, nil, func(context.Context) error {
		probes++
		return probeErr
	})

	r.markDown(errors.New("billing_error"))
	if err := r.ready(context.Background()); err == nil {
		t.Fatal("a model marked down must refuse calls")
	}
	if probes != 0 {
		t.Errorf("probed %d times right after the failure, want 0: probes are rate limited", probes)
	}

	// A minute later a probe is due; it fails, and calls are still refused.
	r.lastProbe = time.Now().Add(-probeEvery)
	if err := r.ready(context.Background()); err == nil || probes != 1 {
		t.Errorf("ready = %v after %d probes, want refused after 1", err, probes)
	}

	// The next due probe succeeds, and the door opens again.
	probeErr = nil
	r.lastProbe = time.Now().Add(-probeEvery)
	if err := r.ready(context.Background()); err != nil {
		t.Errorf("ready = %v after a good probe, want nil", err)
	}
	if err := r.ready(context.Background()); err != nil || probes != 2 {
		t.Errorf("once up, no more probes: ready = %v, probes = %d", err, probes)
	}
}
