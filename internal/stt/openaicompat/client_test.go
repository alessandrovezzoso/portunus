package openaicompat

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests talk to an httptest server on the loopback interface. No real
// STT and no external network is involved.

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: srv.URL + "/", Model: "large-v3-turbo", Language: "it", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestTranscribeSendsAWAVAndReadsTheText(t *testing.T) {
	var fields map[string]string
	var wav []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("request %s %s, want POST /v1/audio/transcriptions", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file field: %v", err)
			return
		}
		wav, _ = io.ReadAll(f)
		_, _ = w.Write([]byte(`{"text":" Buongiorno, sono Mario.\n"}`))
	}))
	defer srv.Close()

	pcm := []int16{100, -200, 300}
	text, err := newClient(t, srv).Transcribe(context.Background(), pcm)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "Buongiorno, sono Mario." {
		t.Errorf("text = %q: whitespace around it must be trimmed", text)
	}
	for k, want := range map[string]string{"model": "large-v3-turbo", "language": "it", "response_format": "json"} {
		if fields[k] != want {
			t.Errorf("field %s = %q, want %q", k, fields[k], want)
		}
	}

	if len(wav) != 44+2*len(pcm) || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("file is not a WAV of %d samples: %d bytes", len(pcm), len(wav))
	}
	if ch, rate := binary.LittleEndian.Uint16(wav[22:]), binary.LittleEndian.Uint32(wav[24:]); ch != 1 || rate != SampleRate {
		t.Errorf("WAV is %d channel(s) at %d Hz, want mono at %d", ch, rate, SampleRate)
	}
	if got := int16(binary.LittleEndian.Uint16(wav[46:])); got != -200 {
		t.Errorf("second sample = %d, want -200", got)
	}
}

func TestTranscribeReportsErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"http status": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"model not loaded"}`, http.StatusServiceUnavailable)
		},
		// whisper.cpp answers some failures with a 200 and an error field.
		"error field": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"error":"failed to read WAV file"}`))
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html>`))
		},
	} {
		srv := httptest.NewServer(handler)
		_, err := newClient(t, srv).Transcribe(context.Background(), make([]int16, 160))
		srv.Close()
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if name != "not json" && !strings.Contains(err.Error(), "model not loaded") && !strings.Contains(err.Error(), "failed to read WAV") {
			t.Errorf("%s: error %q lacks the server's message", name, err)
		}
	}
}

func TestCheckTranscribesOneSecondOfSilence(t *testing.T) {
	var size int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file: %v", err)
			return
		}
		b, _ := io.ReadAll(f)
		size = len(b)
		// Whisper often "hears" something in silence; Check must not care.
		_, _ = w.Write([]byte(`{"text":"Sottotitoli a cura di QTSS"}`))
	}))
	defer srv.Close()
	if err := newClient(t, srv).Check(context.Background()); err != nil {
		t.Errorf("Check on a healthy server: %v", err)
	}
	if size != 44+2*SampleRate {
		t.Errorf("Check sent %d bytes, want one second at %d Hz", size, SampleRate)
	}
}

func TestNewValidates(t *testing.T) {
	for _, opts := range []Options{
		{BaseURL: "127.0.0.1:8890", Model: "m", Language: "it"}, // no scheme
		{BaseURL: "ftp://host", Model: "m", Language: "it"},
		{BaseURL: "http://127.0.0.1:8890", Model: "", Language: "it"},
		{BaseURL: "http://127.0.0.1:8890", Model: "m", Language: ""},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("New(%+v): expected an error", opts)
		}
	}
	c, err := New(Options{BaseURL: "http://127.0.0.1:8890/", Model: "m", Language: "it"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.url != "http://127.0.0.1:8890/v1/audio/transcriptions" {
		t.Errorf("url = %q", c.url)
	}
}

func TestPing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("Ping called %s, want /health", r.URL.Path)
		}
	}))
	defer up.Close()
	if err := newClient(t, up).Ping(context.Background()); err != nil {
		t.Errorf("Ping on a healthy server: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := newClient(t, down).Ping(context.Background()); err == nil {
		t.Error("Ping on a 503 must fail")
	}
}
