package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/config"
)

const resolveTimeout = 5 * time.Second

// preflight runs the network checks that must pass before Portunus
// registers: the registrar must be on a private network, and the bind address
// must be a concrete local address. It returns the address to bind to and the
// registrar's addresses, the only ones SIP requests are accepted from. (The
// provider checks run earlier, in handlerFor, because they depend on the
// mode.)
func preflight(ctx context.Context, cfg *config.Config, log *slog.Logger) (bind netip.Addr, registrar []netip.Addr, err error) {
	host := cfg.SIP.RegistrarHost

	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := resolveHost(rctx, host)
	if err != nil {
		return netip.Addr{}, nil, fmt.Errorf("resolve registrar %q: %w", host, err)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		// A name is checked here, once. The SIP stack resolves it again for
		// every REGISTER, and a DNS change after startup would go unchecked.
		log.Warn("the registrar is a host name: use the S20's IP address instead",
			"registrar", host, "addresses", addrs)
	}

	// The S20 is on the LAN, so a public registrar address almost always
	// means a typo or a hijacked DNS name. Registering there would send our
	// credentials, and later our callers' audio, to someone else.
	if public := nonPrivate(addrs); len(public) > 0 {
		if !cfg.AllowPublicRegistrar {
			return netip.Addr{}, nil, fmt.Errorf(
				"registrar %q resolves to %v, which is not a private address; use the S20's LAN IP, or set %s=true if this is really intended",
				host, public, config.KeyAllowPublicRegistrar)
		}
		log.Warn("registrar is not on a private network, allowed by override",
			"registrar", host, "addresses", public, "override", config.KeyAllowPublicRegistrar)
	}
	first := netip.AddrPortFrom(addrs[0], uint16(cfg.SIP.RegistrarPort))
	log.Info("registrar resolved", "registrar", host, "address", first)

	if cfg.SIP.BindAddr.IsValid() {
		local, err := localAddrs()
		if err != nil {
			return netip.Addr{}, nil, fmt.Errorf("list local addresses: %w", err)
		}
		if err := checkLocal(cfg.SIP.BindAddr, local); err != nil {
			return netip.Addr{}, nil, fmt.Errorf("%s: %w", config.KeySIPBindAddr, err)
		}
		log.Info("bind address", "address", cfg.SIP.BindAddr, "how", "configured")
		return cfg.SIP.BindAddr, addrs, nil
	}

	bind, err = routeSourceAddr(first)
	if err != nil {
		return netip.Addr{}, nil, fmt.Errorf("detect the bind address (set %s to choose it): %w", config.KeySIPBindAddr, err)
	}
	log.Info("bind address", "address", bind, "how", "detected from the route to the registrar")
	return bind, addrs, nil
}

// speechService describes one speech server for preflightService.
type speechService struct {
	name    string // "TTS" or "STT", for the log
	url     string
	check   func(context.Context) error
	timeout time.Duration
	// leaves says what would leave the office if the server were remote.
	leaves string
	// start says how to start the local server.
	start string
}

// preflightService checks a speech server before the first call. Nothing here
// is fatal: an unreachable server is a warning, as CLAUDE.md asks, because it
// may still be starting up.
func preflightService(ctx context.Context, s speechService, log *slog.Logger) {
	if u, err := url.Parse(s.url); err == nil {
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		addrs, err := resolveHost(rctx, u.Hostname())
		cancel()
		switch {
		case err != nil:
			log.Warn(s.name+" host does not resolve", "host", u.Hostname(), "error", err)
		case len(leavesLAN(addrs)) > 0:
			// Kokoro and Whisper run on this host. A remote server would
			// receive what the assistant says or what callers say.
			log.Warn(s.name+" server is outside this host and the LAN: "+s.leaves+" leaves the office",
				"url", s.url, "addresses", leavesLAN(addrs))
		}
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	if err := s.check(cctx); err != nil {
		log.Warn(s.name+" server not reachable; calls will fail until it is ("+s.start+")", "url", s.url, "error", err)
		return
	}
	log.Info(s.name+" server ready", "url", s.url, "check_ms", time.Since(start).Milliseconds())
}

// leavesLAN returns the addresses that are neither on this host (loopback)
// nor on a private network.
func leavesLAN(addrs []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		if !a.IsLoopback() && !a.IsPrivate() {
			out = append(out, a)
		}
	}
	return out
}

// resolveHost returns the addresses of host. An IP literal comes back as is,
// without a DNS query.
func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no addresses")
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, nil
}

// nonPrivate returns the addresses that are not in a private range (RFC 1918
// for IPv4, RFC 4193 for IPv6).
func nonPrivate(addrs []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		if !a.IsPrivate() {
			out = append(out, a)
		}
	}
	return out
}

// routeSourceAddr returns the local address the OS would use to reach dst.
//
// Connecting a UDP socket sends nothing: it only asks the kernel to pick a
// route, and the socket's local address is then the source address of that
// route. That is exactly the address the S20 can reach us on, which is what
// SIP must write into Contact and SDP.
func routeSourceAddr(dst netip.AddrPort) (netip.Addr, error) {
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		return netip.Addr{}, err
	}
	// Nothing was sent, so there is nothing a failed Close could lose.
	defer func() { _ = conn.Close() }()

	local := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if !local.IsValid() || local.IsUnspecified() || local.IsLoopback() {
		return netip.Addr{}, fmt.Errorf("the route to %v leaves from %v, which the PBX cannot reach", dst, local)
	}
	return local, nil
}

// localAddrs lists the addresses assigned to this host's interfaces.
func localAddrs() ([]netip.Addr, error) {
	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range ifaceAddrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if addr, ok := netip.AddrFromSlice(ipnet.IP); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out, nil
}

// checkLocal fails unless addr is assigned to this host. Checking up front
// turns a confusing bind error into one that names the setting to fix.
func checkLocal(addr netip.Addr, local []netip.Addr) error {
	for _, l := range local {
		if l == addr {
			return nil
		}
	}
	return fmt.Errorf("%v is not an address of this host (local addresses: %v)", addr, local)
}
