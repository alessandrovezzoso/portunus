// Package sip registers Portunus with the PBX as one ordinary extension and
// answers the calls the PBX routes to it.
//
// It owns everything that speaks SIP or RTP, and nothing else. Audio leaves
// this package still encoded as G.711 (see Call): decoding and resampling live
// in internal/media. The PBX decides when a call reaches us; there is no
// routing logic here on purpose.
package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
)

// Options configures a Server. cmd/portunus builds it from the config, so this
// package does not depend on internal/config.
type Options struct {
	// BindAddr is the LAN address to listen on. It must be a concrete address,
	// never the wildcard: SIP writes it into Contact and SDP, and the PBX
	// cannot reach 0.0.0.0.
	BindAddr netip.Addr
	Port     int

	RegistrarHost string
	RegistrarPort int

	Extension string
	AuthUser  string
	Password  string

	// UserAgent is sent in the User-Agent header of every REGISTER, so the
	// S20's User Agent Registration Authorization can be turned on.
	UserAgent string

	// RTPPortMin and RTPPortMax bound the local RTP ports, inclusive.
	RTPPortMin int
	RTPPortMax int

	MaxConcurrentCalls int
	RingDelay          time.Duration

	Logger *slog.Logger
}

// shutdownTimeout bounds how long Run waits for active calls to hang up.
const shutdownTimeout = 5 * time.Second

// Server is one registered extension. There must be at most one per process,
// because diago keeps the RTP port range in package-level variables.
type Server struct {
	opts      Options
	log       *slog.Logger
	dg        *diago.Diago
	ua        *sipgo.UserAgent
	registrar sipmsg.Uri

	// mu guards active and closing. A plain counter under a mutex, rather
	// than a channel semaphore, so that "admit a call" and "stop admitting
	// calls" are one atomic decision: no call can slip in after shutdown has
	// started waiting for calls to drain.
	mu      sync.Mutex
	active  int
	closing bool
	calls   sync.WaitGroup
}

// NewServer prepares the SIP stack. It does not touch the network: listening
// and registering start in Run.
func NewServer(opts Options) (*Server, error) {
	if !opts.BindAddr.IsValid() || opts.BindAddr.IsUnspecified() {
		return nil, errors.New("sip: a concrete bind address is required")
	}
	if opts.MaxConcurrentCalls < 1 {
		return nil, errors.New("sip: MaxConcurrentCalls must be at least 1")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	// sipgo uses the user agent "name" as the user part of From and Contact,
	// not as a User-Agent header (it sends none). So the name must be the
	// extension, and the hostname the registrar, giving a From of
	// sip:<extension>@<registrar> as the PBX expects. The real User-Agent
	// header is added to REGISTER in register.go.
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(opts.Extension),
		sipgo.WithUserAgentHostname(opts.RegistrarHost),
	)
	if err != nil {
		return nil, fmt.Errorf("sip: create user agent: %w", err)
	}

	// The libraries log through slog too. Tag them so their lines can be
	// told apart from ours. sipgo's transport layer and diago's media package
	// only read package-level loggers.
	libLog := log.With("component", "diago")
	sipmsg.SetDefaultLogger(libLog)
	media.SetDefaultLogger(libLog)

	// diago reads the RTP port range from package-level variables, so it is
	// set here, once, before any call can allocate a port.
	media.RTPPortStart = opts.RTPPortMin
	media.RTPPortEnd = opts.RTPPortMax

	dg := diago.NewDiago(ua,
		// WithLogger must come first: WithTransport already logs.
		diago.WithLogger(libLog),
		diago.WithTransport(diago.Transport{
			Transport: "udp",
			BindHost:  opts.BindAddr.String(),
			BindPort:  opts.Port,
		}),
		// Pin the codecs. G.711 is all internal/media will speak, and
		// telephone-event keeps RFC 4733 DTMF available for a future
		// "press 1". A-law first, as is usual on European networks.
		diago.WithMediaConfig(diago.MediaConfig{
			Codecs: []media.Codec{media.CodecAudioAlaw, media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
		}),
	)

	return &Server{
		opts: opts,
		log:  log,
		dg:   dg,
		ua:   ua,
		registrar: sipmsg.Uri{
			Scheme: "sip",
			User:   opts.Extension,
			Host:   opts.RegistrarHost,
			Port:   opts.RegistrarPort,
		},
	}, nil
}

// Run listens, keeps the extension registered and hands every answered call
// to h, until ctx is cancelled. It then unregisters, hangs up active calls and
// waits up to shutdownTimeout for them to end.
func (s *Server) Run(ctx context.Context, h Handler) error {
	defer func() {
		if err := s.ua.Close(); err != nil {
			s.log.Warn("closing the SIP stack failed", "error", err)
		}
	}()

	// The listener gets its own context, not ctx. Unregistering and sending
	// BYE at shutdown both need the socket, so it must outlive ctx and close
	// only once everything else is done.
	serveCtx, stopServing := context.WithCancel(context.Background())
	defer stopServing()

	// Listen before registering: once the S20 accepts the REGISTER it may
	// send OPTIONS (its Qualify check) or an INVITE straight away.
	err := s.dg.ServeBackground(serveCtx, func(d *diago.DialogServerSession) {
		s.handleInvite(ctx, d, h)
	})
	if err != nil {
		return fmt.Errorf("sip: listen on %s: %w", netip.AddrPortFrom(s.opts.BindAddr, uint16(s.opts.Port)), err)
	}

	s.registerLoop(ctx) // returns once ctx is done, after unregistering

	s.drainCalls()
	return nil
}

// drainCalls stops admitting calls and waits for the active ones to end. Each
// active call hangs up by itself when ctx is cancelled (see handleInvite).
func (s *Server) drainCalls() {
	s.mu.Lock()
	s.closing = true
	active := s.active
	s.mu.Unlock()

	if active == 0 {
		return
	}
	s.log.Info("waiting for active calls to hang up", "active_calls", active)

	done := make(chan struct{})
	go func() {
		s.calls.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		s.log.Warn("calls still active at shutdown", "timeout", shutdownTimeout)
	}
}

// admit reserves a call slot. It fails when all slots are taken or the server
// is shutting down; the caller then answers 486 Busy Here, which the S20
// forwards to its fallback.
func (s *Server) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.active >= s.opts.MaxConcurrentCalls {
		return false
	}
	s.active++
	s.calls.Add(1)
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.calls.Done()
}
