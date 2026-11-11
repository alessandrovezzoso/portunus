// Package openaicompat is a TTS adapter for servers that implement the
// OpenAI-style speech endpoint, POST /v1/audio/speech. Kokoro-FastAPI, which
// runs Kokoro locally, is one of them, and the same request works with any
// compatible server, so swapping the engine is a configuration change.
//
// The adapter knows nothing about calls or telephony: it returns raw PCM at
// the rate the protocol defines, and internal/media does the rest.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SampleRate is fixed by the protocol: "pcm" is 24 kHz, 16-bit signed,
// little-endian, mono, without a header.
const SampleRate = 24000

// Options configures a Client.
type Options struct {
	// BaseURL is the server root, for example http://127.0.0.1:8880.
	BaseURL string
	Model   string // "kokoro" for Kokoro-FastAPI
	Voice   string // "if_sara": Kokoro infers the Italian pipeline from the "i" prefix

	// HTTPClient is optional; tests use it to point at a local server.
	HTTPClient *http.Client
}

// Client synthesizes speech through one server.
type Client struct {
	speechURL string
	modelsURL string
	model     string
	voice     string
	http      *http.Client
}

// New returns a client for opts. It does not contact the server.
func New(opts Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("tts: %q is not an http(s) URL", opts.BaseURL)
	}
	if opts.Model == "" || opts.Voice == "" {
		return nil, errors.New("tts: model and voice are required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			// The body streams for as long as the speech lasts, so there is no
			// overall timeout. But a server that accepts the connection and
			// never answers must not hang the call, so the headers have a
			// deadline.
			ResponseHeaderTimeout: 10 * time.Second,
		}}
	}
	return &Client{
		speechURL: base.String() + "/v1/audio/speech",
		modelsURL: base.String() + "/v1/models",
		model:     opts.Model,
		voice:     opts.Voice,
		http:      hc,
	}, nil
}

// SampleRate reports the rate of the PCM that Synthesize returns.
func (c *Client) SampleRate() int { return SampleRate }

// speechRequest is the request body. normalization_options is a
// Kokoro-FastAPI extension: its text normalizer is written for English and
// would read Italian numbers wrongly, so it is turned off. Italian
// normalization is Portunus's job. Servers that do not know the field ignore
// it.
type speechRequest struct {
	Model          string        `json:"model"`
	Input          string        `json:"input"`
	Voice          string        `json:"voice"`
	ResponseFormat string        `json:"response_format"`
	Stream         bool          `json:"stream"`
	Normalization  normalization `json:"normalization_options"`
}

type normalization struct {
	Normalize bool `json:"normalize"`
}

// Synthesize asks for text as streaming PCM. The audio starts arriving before
// the whole text is synthesized; the caller must close the returned body.
func (c *Client) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	body, err := json.Marshal(speechRequest{
		Model:          c.model,
		Input:          text,
		Voice:          c.voice,
		ResponseFormat: "pcm",
		Stream:         true,
		Normalization:  normalization{Normalize: false},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.speechURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, statusError(res)
	}
	return res.Body, nil
}

// Check verifies that the server answers. Preflight uses it; a failure is a
// warning there, not a fatal error.
func (c *Client) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsURL, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tts: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return statusError(res)
	}
	return res.Body.Close()
}

// statusError turns a non-200 response into an error that carries the start
// of the server's explanation, and closes the body.
func statusError(res *http.Response) error {
	defer func() { _ = res.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return fmt.Errorf("tts: %s: %s", res.Status, strings.TrimSpace(string(msg)))
}
