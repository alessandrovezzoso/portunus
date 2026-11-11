package sip

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

const noisyMsg = "Reception report SSRC does not match our internal"

// libLogger returns a logger built like the one handed to diago, writing text
// lines at the given minimum level into buf.
func libLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	text := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})
	return slog.New(quietHandler{next: text, noisy: libNoise}).With("component", "diago")
}

func TestQuietHandlerDropsNoiseAtInfo(t *testing.T) {
	var buf bytes.Buffer
	log := libLogger(&buf, slog.LevelInfo)

	log.Warn(noisyMsg, "ssrc", 1)
	if buf.Len() != 0 {
		t.Errorf("noise logged at info level: %q", buf.String())
	}

	log.Warn("Failed to handle request", "req.method", "BYE")
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "component=diago") {
		t.Errorf("an ordinary warning must pass unchanged, with its attributes: %q", buf.String())
	}
}

func TestQuietHandlerKeepsNoiseAtDebug(t *testing.T) {
	var buf bytes.Buffer
	log := libLogger(&buf, slog.LevelDebug)

	log.Warn(noisyMsg, "ssrc", 1)
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "ssrc=1") {
		t.Errorf("noise at debug level: got %q, want it demoted to DEBUG with its attributes", out)
	}
}

func TestQuietHandlerKeepsGroups(t *testing.T) {
	var buf bytes.Buffer
	log := libLogger(&buf, slog.LevelInfo).WithGroup("rtp")

	log.Warn("something else", "port", 20000)
	if !strings.Contains(buf.String(), "rtp.port=20000") {
		t.Errorf("WithGroup lost through the wrapper: %q", buf.String())
	}
}
