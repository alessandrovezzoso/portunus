package sip

import (
	"context"
	"log/slog"
)

// libNoise lists diago and sipgo log messages that are expected when talking
// to the S20 and carry nothing actionable. They are demoted to debug rather
// than dropped, so they are still there when looking for trouble.
//
// They are matched by exact text. If a library upgrade rewords one, it simply
// shows up again at its original level, which is harmless.
var libNoise = map[string]bool{
	// The S20's RTCP receiver reports carry SSRC 1 instead of the SSRC we
	// send with, every 5 seconds. The audio itself is unaffected.
	"Reception report SSRC does not match our internal": true,
	// A duplicate response that arrives after its transaction has ended,
	// typically right after unregistering at shutdown.
	"TransactionLayer: Unhandled sip response. Possible retransmissions. Set UnhandledResponseHandler": true,
}

// quietHandler wraps the slog handler given to the libraries and demotes the
// messages in noisy to debug. Our own log lines never pass through it.
type quietHandler struct {
	next  slog.Handler
	noisy map[string]bool
}

func (h quietHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.noisy[r.Message] && r.Level > slog.LevelDebug {
		// Enabled was asked about the original level, so ask again for the
		// demoted one before handing the record on.
		if !h.next.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
		r.Level = slog.LevelDebug
	}
	return h.next.Handle(ctx, r)
}

func (h quietHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return quietHandler{next: h.next.WithAttrs(attrs), noisy: h.noisy}
}

func (h quietHandler) WithGroup(name string) slog.Handler {
	return quietHandler{next: h.next.WithGroup(name), noisy: h.noisy}
}
