package sip

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	sipmsg "github.com/emiago/sipgo/sip"
)

// fakeTx records the response a middleware sends. Only Respond is used; the
// embedded interface is nil, so anything else would panic and fail the test.
type fakeTx struct {
	sipmsg.ServerTransaction
	res *sipmsg.Response
}

func (f *fakeTx) Respond(res *sipmsg.Response) error {
	f.res = res
	return nil
}

func requestFrom(method sipmsg.RequestMethod, source string) *sipmsg.Request {
	req := notify(method, "", "")
	req.AppendHeader(&sipmsg.FromHeader{Address: req.Recipient, Params: sipmsg.NewParams()})
	req.AppendHeader(&sipmsg.ViaHeader{
		ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP",
		Host: "192.0.2.10", Port: 5060, Params: sipmsg.NewParams(),
	})
	cid := sipmsg.CallIDHeader("test-call")
	req.AppendHeader(&cid)
	req.AppendHeader(&sipmsg.CSeqHeader{SeqNo: 1, MethodName: method})
	if source != "" {
		req.SetSource(source)
	}
	return req
}

func TestOnlyFromLetsThePBXThrough(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := onlyFrom([]netip.Addr{netip.MustParseAddr("192.0.2.10")}, log)

	passed := 0
	h := mw(func(*sipmsg.Request, sipmsg.ServerTransaction) { passed++ })

	tests := []struct {
		name   string
		method sipmsg.RequestMethod
		source string
		pass   bool
	}{
		{"an INVITE from the PBX", sipmsg.INVITE, "192.0.2.10:5060", true},
		{"the PBX's OPTIONS", sipmsg.OPTIONS, "192.0.2.10:5060", true},
		{"the PBX over IPv4-mapped IPv6", sipmsg.INVITE, "[::ffff:192.0.2.10]:5060", true},
		{"an INVITE from another LAN host", sipmsg.INVITE, "192.0.2.66:5060", false},
		{"no recorded source, whatever Via says", sipmsg.INVITE, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := passed
			tx := &fakeTx{}
			h(requestFrom(tt.method, tt.source), tx)
			if got := passed > before; got != tt.pass {
				t.Fatalf("passed = %v, want %v", got, tt.pass)
			}
			if tt.pass && tx.res != nil {
				t.Errorf("a trusted request was answered by the middleware: %v", tx.res.StatusCode)
			}
			if !tt.pass && (tx.res == nil || tx.res.StatusCode != sipmsg.StatusForbidden) {
				t.Errorf("an untrusted request must get 403, got %v", tx.res)
			}
		})
	}
	if !strings.Contains(logs.String(), `level=WARN msg="request refused: not from the PBX" method=INVITE source=192.0.2.66:5060`) {
		t.Errorf("the refusal was not logged:\n%s", logs.String())
	}
}

func TestOnlyFromDropsAnUntrustedACKSilently(t *testing.T) {
	mw := onlyFrom([]netip.Addr{netip.MustParseAddr("192.0.2.10")}, slog.New(slog.DiscardHandler))
	h := mw(func(*sipmsg.Request, sipmsg.ServerTransaction) { t.Error("an untrusted ACK reached diago") })
	tx := &fakeTx{}
	h(requestFrom(sipmsg.ACK, "192.0.2.66:5060"), tx)
	if tx.res != nil {
		t.Errorf("an ACK must get no response, got %d", tx.res.StatusCode)
	}
}

func TestOnlyFromReportsEachSourceOnceAtWarn(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := onlyFrom([]netip.Addr{netip.MustParseAddr("192.0.2.10")}, log)(func(*sipmsg.Request, sipmsg.ServerTransaction) {})
	for range 5 {
		h(requestFrom(sipmsg.INVITE, "192.0.2.66:5060"), &fakeTx{})
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings for one scanner, want 1:\n%s", n, logs.String())
	}
}

func TestRunHandlerTurnsAPanicIntoAnError(t *testing.T) {
	err := runHandler(context.Background(), func(context.Context, *Call) error {
		panic("a bug that must not kill the process")
	}, &Call{})
	if err == nil || !strings.Contains(err.Error(), "handler panicked") {
		t.Fatalf("err = %v, want a panic turned into an error", err)
	}

	want := errors.New("ordinary failure")
	if err := runHandler(context.Background(), func(context.Context, *Call) error { return want }, &Call{}); !errors.Is(err, want) {
		t.Errorf("err = %v, want the handler's own error", err)
	}
}
