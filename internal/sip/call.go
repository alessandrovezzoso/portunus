package sip

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	sipmsg "github.com/emiago/sipgo/sip"
)

// Codec is the G.711 variant negotiated for a call.
type Codec string

const (
	CodecPCMA Codec = "PCMA" // A-law
	CodecPCMU Codec = "PCMU" // µ-law
)

// Call is what this package hands to a mode once a call is answered.
//
// Audio crosses this boundary still encoded, exactly as it travels in RTP.
// Decoding, resampling and encoding belong to internal/media, so this package
// never looks inside a payload.
type Call struct {
	// ID is the SIP Call-ID: the key that ties our log lines to the S20's
	// call records and to a packet capture.
	ID string
	// Caller is the user part of From. It is a third party's phone number:
	// log it through MaskNumber at info level.
	Caller string
	Codec  Codec

	// Inbound yields one RTP payload per Read: normally 20 ms of G.711 from
	// the caller. It returns io.EOF once the call has ended.
	Inbound io.Reader
	// Outbound sends one RTP packet per Write, so each Write must be one
	// 20 ms frame. diago paces it: every Write blocks until the next 20 ms
	// tick of its own clock.
	Outbound io.Writer
}

// InboundBufSize is the buffer a Read on Call.Inbound needs: one full RTP
// packet. diago allocates and copies on every Read into anything smaller.
// It is a variable only because diago's is.
var InboundBufSize = media.RTPBufSize

// Handler runs one answered call. It must return once ctx is cancelled or
// Inbound reports an error; the call is hung up when it returns.
type Handler func(ctx context.Context, c *Call) error

// hangupTimeout bounds the BYE we send when a call is cancelled.
const hangupTimeout = 5 * time.Second

// handleInvite runs one incoming call from INVITE to hangup. diago calls it in
// its own goroutine and hangs the call up once it returns.
func (s *Server) handleInvite(ctx context.Context, d *diago.DialogServerSession, h Handler) {
	callID := ""
	if id := d.InviteRequest.CallID(); id != nil {
		callID = id.Value()
	}
	log := s.log.With("call_id", callID)
	caller := d.FromUser()

	// The concurrency check comes before anything else, so a call beyond the
	// limit is refused before it starts ringing and the S20 can forward it.
	// A call that arrives while shutting down is refused the same way.
	switch {
	case ctx.Err() != nil:
		log.Info("call refused: shutting down", "caller", MaskNumber(caller))
		refuse(d, log)
		return
	case !s.admit():
		log.Warn("call refused: no free call slot",
			"caller", MaskNumber(caller), "max_concurrent_calls", s.opts.MaxConcurrentCalls)
		refuse(d, log)
		return
	}
	defer s.release()

	if err := s.ready(ctx); err != nil {
		log.Warn("call refused: not ready", "caller", MaskNumber(caller), "reason", err)
		refuse(d, log)
		return
	}

	log.Info("incoming call", "caller", MaskNumber(caller))
	log.Debug("incoming call, full caller number", "caller", caller)

	// The call ends when the server shuts down (ctx) or when the dialog ends
	// on the other side (BYE, CANCEL): callCtx covers both.
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopDialogWatch := context.AfterFunc(d.Context(), cancel)
	defer stopDialogWatch()

	if err := d.Trying(); err != nil {
		log.Error("sending 100 Trying failed", "error", err)
		return
	}
	if err := d.Ringing(); err != nil {
		log.Error("sending 180 Ringing failed", "error", err)
		return
	}
	if !sleepCtx(callCtx, s.opts.RingDelay) {
		log.Info("call ended before answer")
		return
	}

	med, err := d.Answer(diago.AnswerOptions{})
	if err != nil {
		// If no final response has gone out yet (an SDP offer diago cannot
		// answer, no free RTP port), refuse the call so the caller reaches
		// voicemail. Once a 200 OK is out, only a BYE can end the call, and
		// diago sends it when this handler returns.
		if d.LoadState() < sipmsg.DialogStateEstablished {
			log.Error("answer failed, call refused", "error", err)
			refuse(d, log)
			return
		}
		log.Error("answer failed", "error", err)
		return
	}

	// A pending Read on the RTP socket has no deadline, so cancelling a
	// context alone would not unblock it. Hanging up does: diago closes the
	// media when the dialog ends, and the Read returns io.EOF.
	stopHangup := context.AfterFunc(callCtx, func() {
		if d.Context().Err() != nil {
			return // the other side already ended the dialog
		}
		hctx, hcancel := context.WithTimeout(context.Background(), hangupTimeout)
		defer hcancel()
		if err := d.Hangup(hctx); err != nil {
			log.Warn("hangup failed", "error", err)
		}
	})
	defer stopHangup()

	codec, err := codecOf(media.CodecAudioFromSession(med.MediaSession()))
	if err != nil {
		log.Error("unexpected codec", "error", err)
		return
	}
	// diago's statistics hook runs after every inbound read and feeds the
	// loss counter reported when the call ends.
	loss := &lossCounter{}
	inbound, err := med.AudioReader(diago.WithAudioReaderRTPStats(func(st media.RTPReadStats) {
		loss.observe(st.FirstPktSequenceNumber, st.LastSequenceNumber, st.PacketsCount)
	}))
	if err != nil {
		log.Error("opening inbound audio failed", "error", err)
		return
	}
	outbound, err := med.AudioWriter()
	if err != nil {
		log.Error("opening outbound audio failed", "error", err)
		return
	}

	// The frame counters sit here, at the RTP boundary, so every mode gets
	// the same end-of-call numbers without counting anything itself.
	in := &countingReader{r: inbound}
	out := &countingWriter{w: outbound}

	start := time.Now()
	log.Info("call answered", "codec", codec)

	herr := runHandler(callCtx, h, &Call{ID: callID, Caller: caller, Codec: codec, Inbound: in, Outbound: out})

	attrs := []any{
		"duration", time.Since(start).Round(time.Millisecond),
		"codec", codec,
		"frames_in", in.frames.Load(),
		"frames_out", out.frames.Load(),
		"lost_pct", loss.lostPct(),
	}
	if herr != nil {
		log.Error("call ended with an error", append(attrs, "error", herr)...)
		return
	}
	log.Info("call ended", attrs...)
}

// runHandler runs h and turns a panic into an error. A bug that crashes one
// call must not take the whole process down, and with it every other call in
// progress. It only covers h's own goroutine: the goroutines a mode starts
// are its own business.
func runHandler(ctx context.Context, h Handler, c *Call) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, c)
}

// ready runs Options.Ready, if set, within readyTimeout.
func (s *Server) ready(ctx context.Context) error {
	if s.opts.Ready == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	return s.opts.Ready(rctx)
}

// refuse turns a call away with 486 Busy Here.
//
// 486 is chosen for what the S20 does with it, not for its literal meaning:
// the PBX applies the extension's When Busy forward, so the caller reaches
// voicemail. diago's default for a call that ends unanswered is 480, which the
// S20 instead announces to the caller as "extension not available".
func refuse(d *diago.DialogServerSession, log *slog.Logger) {
	if err := d.Respond(sipmsg.StatusBusyHere, "Busy Here", nil); err != nil {
		log.Error("sending 486 Busy Here failed", "error", err)
	}
}

// codecOf maps diago's codec to ours. Only G.711 can come back, because the
// codecs are pinned in NewServer; anything else is a bug worth reporting.
func codecOf(c media.Codec) (Codec, error) {
	switch strings.ToLower(c.Name) {
	case "pcma":
		return CodecPCMA, nil
	case "pcmu":
		return CodecPCMU, nil
	}
	return "", fmt.Errorf("negotiated %q (payload type %d), want PCMA or PCMU", c.Name, c.PayloadType)
}

// sleepCtx waits for d, or until ctx is done. It reports whether the full
// wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// MaskNumber hides all but the last three characters of a phone number, for
// logging at info level: "***123". Short values are hidden entirely, so a
// three-digit number is never logged in full.
func MaskNumber(number string) string {
	r := []rune(number)
	if len(r) <= 3 {
		return "***"
	}
	return "***" + string(r[len(r)-3:])
}

// countingReader counts the RTP payloads read through it. The counters are
// atomic because a mode may read and write from different goroutines.
type countingReader struct {
	r      io.Reader
	frames atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.frames.Add(1)
	}
	return n, err
}

// countingWriter counts the RTP packets written through it.
type countingWriter struct {
	w      io.Writer
	frames atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err == nil {
		c.frames.Add(1)
	}
	return n, err
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
