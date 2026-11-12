package sip

import "testing"

// play feeds the counter one packet per sequence number, the way diago's hook
// would: first stays the same, received grows by one each time.
func play(l *lossCounter, first uint16, seqs ...uint16) {
	for i, s := range seqs {
		l.observe(first, s, uint64(i+1))
	}
}

func TestLossCounterNoLoss(t *testing.T) {
	var l lossCounter
	play(&l, 100, 100, 101, 102, 103)
	if got := l.lostPct(); got != 0 {
		t.Errorf("lost %.1f%%, want 0", got)
	}
}

func TestLossCounterCountsGaps(t *testing.T) {
	var l lossCounter
	// 10 expected (100..109), 8 received.
	play(&l, 100, 100, 101, 102, 104, 105, 106, 108, 109)
	if got := l.lostPct(); got != 20 {
		t.Errorf("lost %.1f%%, want 20", got)
	}
}

func TestLossCounterSurvivesTheWrap(t *testing.T) {
	var l lossCounter
	// 65534, 65535, 0, 1, 2: five in a row across the wrap, no loss.
	play(&l, 65534, 65534, 65535, 0, 1, 2)
	if got := l.lostPct(); got != 0 {
		t.Errorf("lost %.1f%% across the wrap, want 0", got)
	}
	// Without the cycle count, the span would look like 65000 packets.
	if l.highest != 1<<16+2 {
		t.Errorf("highest = %d, want %d", l.highest, 1<<16+2)
	}
}

func TestLossCounterIgnoresLatePackets(t *testing.T) {
	var l lossCounter
	// 65535 arrives late, after the wrap: it must not count as a new cycle,
	// nor move the highest back.
	play(&l, 65533, 65533, 65534, 0, 65535, 1)
	if got := l.lostPct(); got != 0 {
		t.Errorf("lost %.1f%%, want 0: reordering is not loss", got)
	}
}

func TestLossCounterWithoutPackets(t *testing.T) {
	var l lossCounter
	if got := l.lostPct(); got != 0 {
		t.Errorf("lost %.1f%% with no packets, want 0", got)
	}
}
