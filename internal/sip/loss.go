package sip

import (
	"math"
	"sync"
)

// lossCounter measures inbound RTP loss from sequence numbers, the way RTCP
// does: packets expected (the span of sequence numbers seen) against packets
// received. Loss here happened on the wire, before this process, so a choppy
// call with lost_pct near zero points at the pipeline instead.
//
// Sequence numbers are 16 bits and wrap every 65536 packets, about 22 minutes
// at 50 packets a second, so the counter extends them with a cycle count.
type lossCounter struct {
	mu       sync.Mutex
	started  bool
	first    uint32 // extended sequence number of the first packet
	highest  uint32 // highest extended sequence number seen
	received uint64
}

// observe records the state after one more packet: the sequence number of the
// first and of the latest packet, and the total received so far. diago's RTP
// statistics hook calls it on every read.
func (l *lossCounter) observe(first, last uint16, received uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		l.started = true
		l.first = uint32(first)
		l.highest = uint32(first)
	}
	l.received = received

	cycles := l.highest &^ 0xFFFF
	low := uint16(l.highest)
	ext := cycles | uint32(last)
	switch {
	case last < low && low-last > 0x8000:
		ext += 1 << 16 // wrapped past 65535
	case last > low && last-low > 0x8000:
		return // a late packet from before the wrap: nothing new
	}
	l.highest = max(l.highest, ext)
}

// lostPct reports the share of expected packets that never arrived, in
// percent, rounded to one decimal. Duplicates can make received exceed
// expected; that counts as no loss.
func (l *lossCounter) lostPct() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		return 0
	}
	expected := uint64(l.highest-l.first) + 1
	if l.received >= expected {
		return 0
	}
	pct := float64(expected-l.received) / float64(expected) * 100
	return math.Round(pct*10) / 10
}
