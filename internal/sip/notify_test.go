package sip

import (
	"testing"

	sipmsg "github.com/emiago/sipgo/sip"
)

// notify builds a request the way the S20 sends it. An empty toTag means the
// request belongs to no dialog; an empty event leaves the Event header out.
func notify(method sipmsg.RequestMethod, toTag, event string) *sipmsg.Request {
	uri := sipmsg.Uri{Scheme: "sip", User: "200", Host: "192.0.2.27", Port: 5060}
	req := sipmsg.NewRequest(method, uri)

	to := &sipmsg.ToHeader{Address: uri, Params: sipmsg.NewParams()}
	if toTag != "" {
		to.Params.Add("tag", toTag)
	}
	req.AppendHeader(to)
	if event != "" {
		req.AppendHeader(sipmsg.NewHeader("Event", event))
	}
	return req
}

func TestIsUnsolicitedMWI(t *testing.T) {
	tests := []struct {
		name string
		req  *sipmsg.Request
		want bool
	}{
		{"the S20's voicemail NOTIFY", notify(sipmsg.NOTIFY, "", "message-summary"), true},
		{"event with parameters", notify(sipmsg.NOTIFY, "", "message-summary;id=1"), true},
		{"event in another case", notify(sipmsg.NOTIFY, "", " Message-Summary "), true},
		{"in-dialog NOTIFY", notify(sipmsg.NOTIFY, "abc123", "message-summary"), false},
		{"another event", notify(sipmsg.NOTIFY, "", "refer"), false},
		{"no Event header", notify(sipmsg.NOTIFY, "", ""), false},
		{"not a NOTIFY", notify(sipmsg.OPTIONS, "", "message-summary"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsolicitedMWI(tt.req); got != tt.want {
				t.Errorf("isUnsolicitedMWI = %v, want %v", got, tt.want)
			}
		})
	}
}
