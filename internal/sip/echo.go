package sip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// Echo is the handler for the echo mode: it sends every inbound RTP payload
// straight back to the caller.
//
// It proves the SIP and RTP legs end to end without touching audio. The bytes
// go back exactly as they arrived, in the codec the call negotiated, so a
// clean echo at the handset rules out registration, SDP, codec negotiation
// and RTP in both directions as the cause of any later audio problem.
//
// No pacing is needed here: inbound packets arrive on the caller's 20 ms
// clock, and diago's writer paces outbound packets on its own.
func Echo(ctx context.Context, c *Call) error {
	buf := make([]byte, InboundBufSize)
	for ctx.Err() == nil {
		n, err := c.Inbound.Read(buf)
		if err != nil {
			if callOver(ctx, err) {
				return nil
			}
			return fmt.Errorf("read inbound audio: %w", err)
		}
		if n == 0 {
			continue // diago reports keepalives and empty payloads as zero-length reads
		}
		if _, err := c.Outbound.Write(buf[:n]); err != nil {
			if callOver(ctx, err) {
				return nil
			}
			return fmt.Errorf("write outbound audio: %w", err)
		}
	}
	return nil
}

// callOver reports whether an I/O error just means the call has ended: the
// context was cancelled, or the media was closed by a hangup on either side.
func callOver(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
