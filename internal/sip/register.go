package sip

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/diago"
	sipmsg "github.com/emiago/sipgo/sip"
)

// Registration retry bounds. An S20 reboot or firmware update must not take
// the service down, so registration never gives up: it backs off from
// registerBackoffMin up to registerBackoffMax and keeps trying.
const (
	registerBackoffMin = time.Second
	registerBackoffMax = 60 * time.Second

	unregisterTimeout = 5 * time.Second
)

// registerLoop keeps the extension registered until ctx is cancelled.
//
// diago's own refresh loop already rides out transient errors, but it gives up
// on the first failed REGISTER, on auth rejections and on most 5xx responses.
// This loop sits on top and retries all of those with exponential backoff.
func (s *Server) registerLoop(ctx context.Context) {
	var wait time.Duration
	for {
		registered, err := s.registerOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		if registered {
			// We were registered and then lost it (an S20 reboot, say). Start
			// the backoff over, so the first retry comes quickly.
			wait = 0
		}
		wait = nextBackoff(wait)

		status := statusCode(err)
		if isAuthRejection(status) {
			// Wrong credentials will not fix themselves, and hammering the
			// S20 with failed attempts can get this host blocked by the PBX's
			// automatic IP blacklist. Wait the maximum straight away.
			wait = registerBackoffMax
		}

		s.log.Error("registration failed",
			"error", err,
			"status", status,
			"registrar", s.registrarAddr(),
			"auth_user", s.opts.AuthUser,
			"retry_in", wait,
		)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// registerOnce sends the initial REGISTER and then keeps it refreshed, until
// a refresh fails or ctx is cancelled. registered reports whether the initial
// REGISTER succeeded, which tells the caller whether to reset the backoff.
func (s *Server) registerOnce(ctx context.Context) (registered bool, err error) {
	tx, err := s.dg.RegisterTransaction(ctx, s.registrar, diago.RegisterOptions{
		Username: s.opts.AuthUser,
		Password: s.opts.Password,
	})
	if err != nil {
		return false, err
	}
	// sipgo sends no User-Agent header of its own. Origin is the request
	// that every refresh and the final unregister reuse, so one header here
	// covers them all.
	tx.Origin.AppendHeader(sipmsg.NewHeader("User-Agent", s.opts.UserAgent))

	if err := tx.Register(ctx); err != nil {
		return false, err
	}
	s.log.Info("registered",
		"registrar", s.registrarAddr(),
		"extension", s.opts.Extension,
		"contact", s.contactAddr(),
	)

	err = tx.QualifyLoop(ctx)
	if ctx.Err() != nil {
		// Shutting down. ctx is already cancelled, so unregister with a fresh
		// one: this is what makes the S20 send calls to the fallback right
		// away instead of to a dead contact until the registration expires.
		uctx, cancel := context.WithTimeout(context.Background(), unregisterTimeout)
		defer cancel()
		if uerr := tx.Unregister(uctx); uerr != nil {
			s.log.Error("unregister failed", "error", uerr, "status", statusCode(uerr))
		} else {
			s.log.Info("unregistered", "registrar", s.registrarAddr())
		}
	}
	return true, err
}

// nextBackoff doubles the previous wait, starting at registerBackoffMin and
// capped at registerBackoffMax.
func nextBackoff(prev time.Duration) time.Duration {
	if prev < registerBackoffMin {
		return registerBackoffMin
	}
	return min(2*prev, registerBackoffMax)
}

// statusCode returns the SIP status of a rejected REGISTER, or 0 when the
// error is not a SIP response (a timeout or a network error, for example).
func statusCode(err error) int {
	var rr *diago.RegisterResponseError
	if errors.As(err, &rr) && rr.RegisterRes != nil {
		return rr.RegisterRes.StatusCode
	}
	return 0
}

// isAuthRejection reports whether status means the PBX refused our
// credentials, as opposed to being unreachable or temporarily unavailable.
func isAuthRejection(status int) bool {
	switch status {
	case sipmsg.StatusUnauthorized, sipmsg.StatusForbidden, sipmsg.StatusProxyAuthRequired:
		return true
	}
	return false
}

func (s *Server) registrarAddr() string {
	return joinHostPort(s.opts.RegistrarHost, s.opts.RegistrarPort)
}

func (s *Server) contactAddr() string {
	return joinHostPort(s.opts.BindAddr.String(), s.opts.Port)
}
