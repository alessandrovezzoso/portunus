// Package openaicompat is an STT adapter for servers that implement the
// OpenAI-style transcription endpoint, POST /v1/audio/transcriptions.
// whisper.cpp's server is one of them when started with --inference-path, and
// the same request works with any compatible server (speaches, and others),
// so swapping the engine is a configuration change.
//
// The adapter knows nothing about calls or telephony: it takes 16-bit PCM at
// the rate it declares, and internal/media does every conversion, including
// the WAV container.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
)

// SampleRate is Whisper's own rate. Sending anything else would only make the
// server resample it.
const SampleRate = 16000

// Options configures a Client.
type Options struct {
	// BaseURL is the server root, for example http://127.0.0.1:8890.
	BaseURL string
	// Model is sent as the "model" field. whisper.cpp ignores it, because its
	// model is chosen when the server starts; other servers require it.
	Model string
	// Language is the ISO 639-1 code of what callers speak: "it". Naming it
	// skips detection, which costs time and can guess wrong on a short phrase.
	Language string

	// HTTPClient is optional; tests use it to point at a local server.
	HTTPClient *http.Client
}

// Client transcribes utterances through one server.
type Client struct {
	url      string
	model    string
	language string
	http     *http.Client
}

// New returns a client for opts. It does not contact the server.
func New(opts Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("stt: %q is not an http(s) URL", opts.BaseURL)
	}
	if opts.Model == "" || opts.Language == "" {
		return nil, errors.New("stt: model and language are required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
			// A backstop only: internal/call bounds each request more
			// tightly through its context. Whisper on a CPU can take several
			// seconds, so this is generous.
			Timeout: time.Minute,
		}
	}
	return &Client{
		url:      base.String() + "/v1/audio/transcriptions",
		model:    opts.Model,
		language: opts.Language,
		http:     hc,
	}, nil
}

// SampleRate reports the rate of the PCM that Transcribe expects.
func (c *Client) SampleRate() int { return SampleRate }

// transcription is the JSON response. whisper.cpp reports some failures with
// a 200 and an "error" field instead of an HTTP status, so both are read.
type transcription struct {
	Text  string `json:"text"`
	Error string `json:"error"`
}

// Transcribe uploads pcm, 16-bit mono at SampleRate, as a WAV file and returns
// the text.
func (c *Client) Transcribe(ctx context.Context, pcm []int16) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err := file.Write(media.EncodeWAV(pcm, SampleRate)); err != nil {
		return "", err
	}
	for k, v := range map[string]string{
		"model":           c.model,
		"language":        c.language,
		"response_format": "json",
	} {
		if err := form.WriteField(k, v); err != nil {
			return "", err
		}
	}
	if err := form.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("stt: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("stt: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
	var t transcription
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("stt: decode response: %w", err)
	}
	if t.Error != "" {
		return "", fmt.Errorf("stt: server error: %s", t.Error)
	}
	return strings.TrimSpace(t.Text), nil
}

// Check transcribes one second of silence. That proves in one request that the
// server answers and that its model is loaded, and it warms the model up, so
// the first call does not pay for a cold start. The text is ignored: Whisper
// may well invent something for silence.
//
// Preflight uses it; a failure is a warning there, not a fatal error.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.Transcribe(ctx, make([]int16, SampleRate))
	return err
}
