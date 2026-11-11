package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests talk to an httptest server on the loopback interface. No real
// TTS and no external network is involved.

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: srv.URL + "/", Model: "kokoro", Voice: "if_sara", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestSynthesizeSendsTheRequestAndStreamsTheBody(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3} // an odd byte count, as a streamed chunk can end
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/speech" {
			t.Errorf("request %s %s, want POST /v1/audio/speech", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(pcm[:3])
		w.(http.Flusher).Flush() // force a chunk boundary inside a sample
		_, _ = w.Write(pcm[3:])
	}))
	defer srv.Close()

	body, err := newClient(t, srv).Synthesize(context.Background(), "Buongiorno.")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != string(pcm) {
		t.Errorf("body = %v, want %v", data, pcm)
	}

	want := map[string]any{
		"model": "kokoro", "input": "Buongiorno.", "voice": "if_sara",
		"response_format": "pcm", "stream": true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request %s = %v, want %v", k, got[k], v)
		}
	}
	norm, _ := got["normalization_options"].(map[string]any)
	if norm == nil || norm["normalize"] != false {
		t.Errorf("normalization_options = %v, want normalize false", got["normalization_options"])
	}
}

func TestSynthesizeReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"voice not found"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := newClient(t, srv).Synthesize(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "voice not found") {
		t.Errorf("error = %v, want the status and the server's message", err)
	}
}

func TestCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("Check called %s, want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer ok.Close()
	if err := newClient(t, ok).Check(context.Background()); err != nil {
		t.Errorf("Check on a healthy server: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := newClient(t, down).Check(context.Background()); err == nil {
		t.Error("Check on a 503 must fail")
	}
}

func TestNewValidates(t *testing.T) {
	for _, opts := range []Options{
		{BaseURL: "127.0.0.1:8880", Model: "kokoro", Voice: "if_sara"}, // no scheme
		{BaseURL: "ftp://host", Model: "kokoro", Voice: "if_sara"},
		{BaseURL: "http://127.0.0.1:8880", Model: "", Voice: "if_sara"},
		{BaseURL: "http://127.0.0.1:8880", Model: "kokoro", Voice: ""},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("New(%+v): expected an error", opts)
		}
	}
	c, err := New(Options{BaseURL: "http://127.0.0.1:8880/", Model: "kokoro", Voice: "if_sara"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.speechURL != "http://127.0.0.1:8880/v1/audio/speech" {
		t.Errorf("speechURL = %q", c.speechURL)
	}
	if c.SampleRate() != 24000 {
		t.Errorf("SampleRate = %d, want 24000", c.SampleRate())
	}
}
