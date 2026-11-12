package sip

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/emiago/diago/media"
)

// mediaFromPeer drops inbound RTP that does not come from the address the
// PBX named in its SDP.
//
// diago reads RTP from whoever sends it to the call's port: it records the
// sender, but does not compare it with the negotiated address. Without this
// check, anything on the LAN that finds the port could speak into the call,
// and its words would end up in the transcript and in the message.
//
// The address comes from the PBX's own SDP, which arrived over SIP from a
// trusted source (see onlyFrom), so nothing needs configuring: the S20's IP
// is written once, in PORTUNUS_SIP_REGISTRAR. Only the IP is compared, not
// the port, because a PBX may send from a port other than the one it
// receives on.
type mediaFromPeer struct {
	r    io.Reader
	sess *media.MediaSession

	dropped atomic.Int64
	mu      sync.Mutex
	first   string // the first unexpected source, for the log
}

// Read returns the next payload, or an empty read when the packet came from
// elsewhere: every mode skips empty reads, as it does keepalives.
func (m *mediaFromPeer) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n == 0 || m.fromPeer() {
		return n, err
	}
	if m.dropped.Add(1) == 1 {
		m.mu.Lock()
		m.first = fmt.Sprint(m.sess.ReadRTPFromAddr)
		m.mu.Unlock()
	}
	return 0, err
}

// fromPeer checks the packet just read. diago sets ReadRTPFromAddr on every
// read; it is valid only right after it, in the same goroutine, as here.
func (m *mediaFromPeer) fromPeer() bool {
	from, ok := m.sess.ReadRTPFromAddr.(*net.UDPAddr)
	return ok && from.IP.Equal(m.sess.Raddr.IP)
}

// firstSource reports the first unexpected source, if any.
func (m *mediaFromPeer) firstSource() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.first
}
