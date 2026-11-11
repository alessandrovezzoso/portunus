package sip

import (
	"log/slog"
	"strings"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
)

// answerUnsolicitedMWI is a diago request middleware that acknowledges
// message-waiting NOTIFYs arriving outside any dialog.
//
// The S20 sends one to the extension about every 30 seconds, with
// "Event: message-summary", to report the state of its voicemail box. RFC 3842
// allows this without a subscription. diago only accepts NOTIFY inside a
// dialog, so it answers 481 and logs a warning every time. Portunus has no use
// for the voicemail state, but 200 OK is the correct answer, and it keeps the
// log readable. Every other request goes on to diago untouched.
func answerUnsolicitedMWI(log *slog.Logger) func(next sipgo.RequestHandler) sipgo.RequestHandler {
	return func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
			if !isUnsolicitedMWI(req) {
				next(req, tx)
				return
			}
			res := sipmsg.NewResponseFromRequest(req, sipmsg.StatusOK, "OK", nil)
			if err := tx.Respond(res); err != nil {
				log.Warn("answering a voicemail NOTIFY failed", "error", err)
			}
		}
	}
}

// isUnsolicitedMWI reports whether req is a message-summary NOTIFY that
// belongs to no dialog, which shows as a To header without a tag.
func isUnsolicitedMWI(req *sipmsg.Request) bool {
	if req.Method != sipmsg.NOTIFY {
		return false
	}
	to := req.To()
	if to == nil {
		return false
	}
	if tag, ok := to.Params.Get("tag"); ok && tag != "" {
		return false // in-dialog: diago's business
	}
	event := req.GetHeader("Event")
	if event == nil {
		return false
	}
	// The event may carry parameters, as in "message-summary;id=1".
	name, _, _ := strings.Cut(event.Value(), ";")
	return strings.EqualFold(strings.TrimSpace(name), "message-summary")
}
