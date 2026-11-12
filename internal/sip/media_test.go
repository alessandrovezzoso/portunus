package sip

import (
	"io"
	"net"
	"testing"

	"github.com/emiago/diago/media"
)

// fakeRTP plays one payload per source, and records each source in the
// session the way diago does on every read.
type fakeRTP struct {
	sess    *media.MediaSession
	sources []string
	next    int
}

func (f *fakeRTP) Read(p []byte) (int, error) {
	if f.next >= len(f.sources) {
		return 0, io.EOF
	}
	src := f.sources[f.next]
	f.next++
	f.sess.ReadRTPFromAddr = nil
	if src != "" {
		addr, err := net.ResolveUDPAddr("udp", src)
		if err != nil {
			return 0, err
		}
		f.sess.ReadRTPFromAddr = addr
	}
	return copy(p, []byte{0xff, 0xff, 0xff}), nil
}

func TestMediaFromPeerDropsOtherSources(t *testing.T) {
	sess := &media.MediaSession{Raddr: net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 10000}}
	peer := &mediaFromPeer{sess: sess, r: &fakeRTP{sess: sess, sources: []string{
		"192.0.2.10:10000",          // the PBX, as in its SDP
		"192.0.2.66:40000",          // another LAN host: dropped
		"192.0.2.10:10002",          // the PBX from another port: kept
		"[::ffff:192.0.2.10]:10000", // the PBX, IPv4-mapped
		"",                          // no recorded source: dropped
	}}}

	buf := make([]byte, 16)
	var kept []int
	for i := 0; ; i++ {
		n, err := peer.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if n > 0 {
			kept = append(kept, i)
		}
	}

	if len(kept) != 3 || kept[0] != 0 || kept[1] != 2 || kept[2] != 3 {
		t.Errorf("kept reads %v, want [0 2 3]", kept)
	}
	if got := peer.dropped.Load(); got != 2 {
		t.Errorf("dropped %d packets, want 2", got)
	}
	if got := peer.firstSource(); got != "192.0.2.66:40000" {
		t.Errorf("first unexpected source %q, want 192.0.2.66:40000", got)
	}
}
