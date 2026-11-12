package sip

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
)

// middleware is diago's request middleware: it sees every request before
// diago does, and decides whether diago sees it at all.
type middleware = func(next sipgo.RequestHandler) sipgo.RequestHandler

// chain runs outer first, then inner, then diago.
func chain(outer, inner middleware) middleware {
	return func(next sipgo.RequestHandler) sipgo.RequestHandler { return outer(inner(next)) }
}

// maxUntrustedLogged bounds the memory spent remembering which untrusted
// addresses were already reported.
const maxUntrustedLogged = 1024

// onlyFrom refuses every request that does not come from one of the trusted
// addresses: the PBX's.
//
// Without it, anything on the LAN that reaches the SIP port could call
// Portunus directly, bypassing the S20: spending model credit, taking the call
// slots real callers need, and having Portunus stream audio to whatever
// address its SDP names. The check uses the packet's source address, as the
// transport recorded it, never the Via header, which the sender writes. UDP
// lets an attacker forge that address, but then every answer goes to the PBX,
// not to them. It is defence in depth, behind the host firewall and the S20's
// own IP restriction.
func onlyFrom(trusted []netip.Addr, log *slog.Logger) middleware {
	allowed := make(map[netip.Addr]bool, len(trusted))
	for _, a := range trusted {
		allowed[a.Unmap()] = true
	}

	// Each new address is reported once at warn level, then at debug: a SIP
	// scanner sends thousands of requests and must not flood the log.
	var mu sync.Mutex
	reported := make(map[netip.Addr]bool)
	report := func(req *sipmsg.Request, src netip.Addr) {
		mu.Lock()
		first := !reported[src] && len(reported) < maxUntrustedLogged
		if first {
			reported[src] = true
		}
		mu.Unlock()
		level := slog.LevelDebug
		if first {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "request refused: not from the PBX",
			"method", req.Method.String(), "source", req.MessageData.Source())
	}

	return func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
			src, ok := sourceAddr(req)
			if ok && allowed[src] {
				next(req, tx)
				return
			}
			report(req, src)
			if req.Method == sipmsg.ACK {
				return // an ACK gets no response
			}
			res := sipmsg.NewResponseFromRequest(req, sipmsg.StatusForbidden, "Forbidden", nil)
			if err := tx.Respond(res); err != nil {
				log.Debug("answering an untrusted request failed", "error", err)
			}
		}
	}
}

// sourceAddr returns the address a request arrived from, as the transport
// recorded it. A request with no recorded source is not trusted.
func sourceAddr(req *sipmsg.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(req.MessageData.Source())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}
