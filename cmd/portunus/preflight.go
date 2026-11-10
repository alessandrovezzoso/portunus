package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/config"
)

const resolveTimeout = 5 * time.Second

// preflight runs the checks that must pass before Portunus registers, and
// returns the LAN address to bind to.
//
// For now that means the network: the registrar must be on a private network,
// and the bind address must be a concrete local address. Provider credential
// checks join here in later milestones.
func preflight(ctx context.Context, cfg *config.Config, log *slog.Logger) (netip.Addr, error) {
	host := cfg.SIP.RegistrarHost

	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := resolveHost(rctx, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve registrar %q: %w", host, err)
	}

	// The S20 is on the LAN, so a public registrar address almost always
	// means a typo or a hijacked DNS name. Registering there would send our
	// credentials, and later our callers' audio, to someone else.
	if public := nonPrivate(addrs); len(public) > 0 {
		if !cfg.AllowPublicRegistrar {
			return netip.Addr{}, fmt.Errorf(
				"registrar %q resolves to %v, which is not a private address; use the S20's LAN IP, or set %s=true if this is really intended",
				host, public, config.KeyAllowPublicRegistrar)
		}
		log.Warn("registrar is not on a private network, allowed by override",
			"registrar", host, "addresses", public, "override", config.KeyAllowPublicRegistrar)
	}
	registrar := netip.AddrPortFrom(addrs[0], uint16(cfg.SIP.RegistrarPort))
	log.Info("registrar resolved", "registrar", host, "address", registrar)

	if cfg.SIP.BindAddr.IsValid() {
		local, err := localAddrs()
		if err != nil {
			return netip.Addr{}, fmt.Errorf("list local addresses: %w", err)
		}
		if err := checkLocal(cfg.SIP.BindAddr, local); err != nil {
			return netip.Addr{}, fmt.Errorf("%s: %w", config.KeySIPBindAddr, err)
		}
		log.Info("bind address", "address", cfg.SIP.BindAddr, "how", "configured")
		return cfg.SIP.BindAddr, nil
	}

	bind, err := routeSourceAddr(registrar)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("detect the bind address (set %s to choose it): %w", config.KeySIPBindAddr, err)
	}
	log.Info("bind address", "address", bind, "how", "detected from the route to the registrar")
	return bind, nil
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
