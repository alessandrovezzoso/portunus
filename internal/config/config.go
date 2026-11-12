// Package config turns flags, the .env file and the process environment into
// one validated Config.
//
// Precedence is flag > .env > shell environment. The file beats the shell on
// purpose: a stale `set -a && . ./.env` export must never silently override a
// file that has since been edited. Load is a pure function of its Inputs and
// never touches the filesystem or os.Getenv, so the precedence rules can be
// tested exhaustively and live in exactly one place.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Setting names. They are the only place these strings are spelled out.
const (
	KeyEnvFile = "PORTUNUS_ENV_FILE" // read by cmd/portunus before Load runs

	KeyMode     = "PORTUNUS_MODE"
	KeyLogLevel = "PORTUNUS_LOG_LEVEL"

	KeySIPRegistrar = "PORTUNUS_SIP_REGISTRAR"
	KeySIPExtension = "PORTUNUS_SIP_EXTENSION"
	KeySIPAuthUser  = "PORTUNUS_SIP_AUTH_USER"
	KeySIPPassword  = "PORTUNUS_SIP_PASSWORD"
	KeySIPBindAddr  = "PORTUNUS_SIP_BIND_ADDR"
	KeySIPPort      = "PORTUNUS_SIP_PORT"

	KeyRTPPortMin = "PORTUNUS_RTP_PORT_MIN"
	KeyRTPPortMax = "PORTUNUS_RTP_PORT_MAX"

	KeyMaxConcurrentCalls   = "PORTUNUS_MAX_CONCURRENT_CALLS"
	KeyRingDelay            = "PORTUNUS_RING_DELAY"
	KeyAllowPublicRegistrar = "PORTUNUS_ALLOW_PUBLIC_REGISTRAR"

	KeyTTSURL       = "PORTUNUS_TTS_URL"
	KeyTTSModel     = "PORTUNUS_TTS_MODEL"
	KeyTTSVoice     = "PORTUNUS_TTS_VOICE"
	KeyOutputBuffer = "PORTUNUS_OUTPUT_BUFFER"
	KeyGreetingFile = "PORTUNUS_GREETING_FILE"

	KeySTTURL          = "PORTUNUS_STT_URL"
	KeySTTModel        = "PORTUNUS_STT_MODEL"
	KeySTTMaxUtterance = "PORTUNUS_STT_MAX_UTTERANCE"
	KeyVADThreshold    = "PORTUNUS_VAD_THRESHOLD"
	KeyVADSilence      = "PORTUNUS_VAD_SILENCE"

	// KeyAnthropicAPIKey keeps the name the official SDK expects, without
	// the PORTUNUS_ prefix.
	KeyAnthropicAPIKey = "ANTHROPIC_API_KEY"
	KeyLLMModel        = "PORTUNUS_LLM_MODEL"
	KeyLLMMaxTokens    = "PORTUNUS_LLM_MAX_TOKENS"
	KeyLLMThinking     = "PORTUNUS_LLM_THINKING"

	KeyMaxCallTime = "PORTUNUS_MAX_CALL_TIME"
	KeyMaxTurns    = "PORTUNUS_MAX_TURNS"

	KeySystemPromptFile  = "PORTUNUS_SYSTEM_PROMPT_FILE"
	KeyAgentGreetingFile = "PORTUNUS_AGENT_GREETING_FILE"
	KeyHoldingFile       = "PORTUNUS_HOLDING_FILE"
)

// knownKeys lists every PORTUNUS_ variable this version understands. Anything
// else with the prefix is reported as a warning, which is how a typo such as
// PORTUNUS_SIP_PASWORD gets noticed instead of silently ignored.
var knownKeys = []string{
	KeyEnvFile,
	KeyMode, KeyLogLevel,
	KeySIPRegistrar, KeySIPExtension, KeySIPAuthUser, KeySIPPassword, KeySIPBindAddr, KeySIPPort,
	KeyRTPPortMin, KeyRTPPortMax,
	KeyMaxConcurrentCalls, KeyRingDelay, KeyAllowPublicRegistrar,
	KeyTTSURL, KeyTTSModel, KeyTTSVoice, KeyOutputBuffer, KeyGreetingFile,
	KeySTTURL, KeySTTModel, KeySTTMaxUtterance, KeyVADThreshold, KeyVADSilence,
	KeyLLMModel, KeyLLMMaxTokens, KeyLLMThinking, KeyMaxCallTime, KeyMaxTurns,
	KeySystemPromptFile, KeyAgentGreetingFile, KeyHoldingFile,
}

// secretKeys appear in Summary only as their Fingerprint.
var secretKeys = map[string]bool{KeySIPPassword: true, KeyAnthropicAPIKey: true}

const keyPrefix = "PORTUNUS_"

// Mode selects what Portunus does with an answered call. The early milestones
// stay available as diagnostic modes.
type Mode string

const (
	ModeEcho   Mode = "echo"   // RTP loopback: proves SIP, SDP and RTP both ways
	ModeGreet  Mode = "greet"  // TTS greeting: proves the outbound audio path
	ModeListen Mode = "listen" // VAD + STT: proves the inbound audio path
	ModeAgent  Mode = "agent"  // the full conversation
)

// Source says where a setting's effective value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceEnv     Source = "env"
	SourceDotEnv  Source = ".env"
	SourceFlag    Source = "flag"
)

// Inputs are the three places a setting can come from, highest precedence
// first. Each map is keyed by variable name, flags included: cmd/portunus
// stores the -mode flag under KeyMode, and only when the flag was given.
type Inputs struct {
	Flags  map[string]string
	DotEnv map[string]string
	Env    map[string]string
}

// Config is the validated configuration.
type Config struct {
	Mode     Mode
	LogLevel slog.Level

	SIP SIP

	// RTPPortMin and RTPPortMax bound the local RTP ports, inclusive. Every
	// call takes two of them, because diago binds RTCP on the RTP port + 1.
	RTPPortMin int
	RTPPortMax int

	MaxConcurrentCalls int
	RingDelay          time.Duration

	AllowPublicRegistrar bool

	TTS TTS

	// OutputBuffer caps the audio queued ahead of the RTP clock. It must hold
	// a whole reply, or speech is cut mid-utterance.
	OutputBuffer time.Duration

	// GreetingFile holds the text spoken in greet mode. It lives in prompts/,
	// which git ignores, because the real text names the office.
	GreetingFile string

	STT STT
	VAD VAD
	LLM LLM

	// MaxCallTime and MaxTurns are the per-call cost ceilings of the agent
	// mode. Either one ends the call gracefully.
	MaxCallTime time.Duration
	MaxTurns    int

	// The texts of the agent mode. They live in prompts/, which git ignores,
	// because the real ones name the office.
	SystemPromptFile  string
	AgentGreetingFile string
	HoldingFile       string

	values  map[string]string // effective value of each setting, for Summary
	sources map[string]Source
}

// SIP holds the settings for the registration with the PBX.
type SIP struct {
	RegistrarHost string
	RegistrarPort int

	Extension string
	// AuthUser is the Registration Name used for digest auth. It defaults to
	// the extension number, but the S20 lets the two differ.
	AuthUser string
	Password string

	// BindAddr is the local address to listen on. The zero value means
	// "resolve it at startup from the route to the registrar".
	BindAddr netip.Addr
	Port     int
}

// TTS points at the speech synthesis service: a server speaking the
// OpenAI-style /v1/audio/speech API, Kokoro-FastAPI by default.
type TTS struct {
	URL   string
	Model string
	Voice string
}

// STT points at the speech recognition service: a server speaking the
// OpenAI-style /v1/audio/transcriptions API, whisper.cpp by default.
type STT struct {
	URL   string
	Model string
	// MaxUtterance cuts longer speech into pieces. It must fit the server's
	// input window, or the end of each long utterance is silently lost: about
	// 10 s with the --audio-ctx 512 that deploy/compose.yaml sets, 30 s with
	// Whisper's full window.
	MaxUtterance time.Duration
}

// LLM configures the conversation model, Claude.
type LLM struct {
	APIKey string
	// Model is set here and nowhere else. A fast Haiku-class model is the
	// default: on the phone, latency beats eloquence.
	Model string
	// MaxTokens caps each reply, thinking included: a cost ceiling.
	MaxTokens int64
	// Thinking lets the model reason before answering, at the cost of
	// silence before its first word.
	Thinking bool
}

// VAD tunes the detection of the caller's speech.
type VAD struct {
	// Threshold is the speech probability at which an utterance starts.
	// Raising it is the fix for a handset that echoes the assistant back.
	Threshold float64
	// Silence is how long the caller must pause for an utterance to end.
	// Every turn waits this long before the STT starts, so it is the main
	// latency knob on the listening side.
	Silence time.Duration
}

const defaultSIPPort = 5060

// Load builds a Config from in. It returns the warnings worth logging (unknown
// keys) separately, because the logger cannot exist until the log level has
// been read.
func Load(in Inputs) (*Config, []string, error) {
	l := &loader{in: in, values: map[string]string{}, sources: map[string]Source{}}

	cfg := &Config{
		Mode: Mode(strings.ToLower(l.string(KeyMode, string(ModeEcho)))),
		SIP: SIP{
			Extension: l.string(KeySIPExtension, ""),
			Password:  l.string(KeySIPPassword, ""),
			Port:      l.int(KeySIPPort, defaultSIPPort),
		},
		RTPPortMin:           l.int(KeyRTPPortMin, 20000),
		RTPPortMax:           l.int(KeyRTPPortMax, 20099),
		MaxConcurrentCalls:   l.int(KeyMaxConcurrentCalls, 2),
		RingDelay:            l.duration(KeyRingDelay, 0),
		AllowPublicRegistrar: l.bool(KeyAllowPublicRegistrar, false),
		TTS: TTS{
			URL:   l.string(KeyTTSURL, "http://127.0.0.1:8880"),
			Model: l.string(KeyTTSModel, "kokoro"),
			Voice: l.string(KeyTTSVoice, "if_sara"),
		},
		OutputBuffer: l.duration(KeyOutputBuffer, 10*time.Second),
		GreetingFile: l.string(KeyGreetingFile, "prompts/greeting.txt"),
		STT: STT{
			URL:          l.string(KeySTTURL, "http://127.0.0.1:8890"),
			Model:        l.string(KeySTTModel, "large-v3-turbo"),
			MaxUtterance: l.duration(KeySTTMaxUtterance, 9500*time.Millisecond),
		},
		VAD: VAD{
			Threshold: l.float(KeyVADThreshold, 0.5),
			Silence:   l.duration(KeyVADSilence, 700*time.Millisecond),
		},
		LLM: LLM{
			APIKey:    l.string(KeyAnthropicAPIKey, ""),
			Model:     l.string(KeyLLMModel, "claude-haiku-5-5"),
			MaxTokens: int64(l.int(KeyLLMMaxTokens, 1024)),
			Thinking:  l.bool(KeyLLMThinking, false),
		},
		MaxCallTime:       l.duration(KeyMaxCallTime, 5*time.Minute),
		MaxTurns:          l.int(KeyMaxTurns, 20),
		SystemPromptFile:  l.string(KeySystemPromptFile, "prompts/system.txt"),
		AgentGreetingFile: l.string(KeyAgentGreetingFile, "prompts/agent-greeting.txt"),
		HoldingFile:       l.string(KeyHoldingFile, "prompts/holding.txt"),
	}
	cfg.SIP.AuthUser = l.string(KeySIPAuthUser, cfg.SIP.Extension)
	cfg.LogLevel = l.logLevel(KeyLogLevel, slog.LevelInfo)
	cfg.SIP.RegistrarHost, cfg.SIP.RegistrarPort = l.hostPort(KeySIPRegistrar)
	cfg.SIP.BindAddr = l.addr(KeySIPBindAddr)

	cfg.values, cfg.sources = l.values, l.sources

	if err := errors.Join(append(l.errs, cfg.validate()...)...); err != nil {
		return nil, nil, err
	}
	return cfg, unknownKeys(in), nil
}

func (c *Config) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Mode {
	case ModeEcho, ModeGreet, ModeListen, ModeAgent:
	default:
		add("%s: unknown mode %q (want echo, greet, listen or agent)", KeyMode, c.Mode)
	}

	if c.SIP.RegistrarHost == "" {
		add("%s is required (the S20's LAN address)", KeySIPRegistrar)
	}
	if c.SIP.Extension == "" {
		add("%s is required", KeySIPExtension)
	} else if strings.ContainsAny(c.SIP.Extension, " \t@:;") {
		add("%s: %q is not a valid SIP user", KeySIPExtension, c.SIP.Extension)
	}
	if c.SIP.Password == "" {
		add("%s is required", KeySIPPassword)
	}
	if !validPort(c.SIP.Port) {
		add("%s: %d is not a valid port", KeySIPPort, c.SIP.Port)
	}

	if !validPort(c.RTPPortMin) || !validPort(c.RTPPortMax) || c.RTPPortMax <= c.RTPPortMin {
		add("%s/%s: %d-%d is not a valid port range", KeyRTPPortMin, KeyRTPPortMax, c.RTPPortMin, c.RTPPortMax)
	}
	if c.MaxConcurrentCalls < 1 {
		add("%s must be at least 1", KeyMaxConcurrentCalls)
	} else if slots := (c.RTPPortMax - c.RTPPortMin + 1) / 2; slots < c.MaxConcurrentCalls {
		add("%s/%s: the range fits %d calls (two ports each), fewer than %s=%d",
			KeyRTPPortMin, KeyRTPPortMax, slots, KeyMaxConcurrentCalls, c.MaxConcurrentCalls)
	}
	if c.RingDelay < 0 {
		add("%s must not be negative", KeyRingDelay)
	}

	for _, s := range []struct{ key, url string }{{KeyTTSURL, c.TTS.URL}, {KeySTTURL, c.STT.URL}} {
		if u, err := url.Parse(s.url); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("%s: %q is not an http(s) URL", s.key, s.url)
		}
	}
	// A second of audio is the shortest sentence worth saying; below that,
	// replies would be cut before they start.
	if c.OutputBuffer < time.Second {
		add("%s must be at least 1s (it must hold a whole reply)", KeyOutputBuffer)
	}

	// Whisper's window is 30 s at most; under 2 s, ordinary sentences would be
	// chopped into fragments.
	if c.STT.MaxUtterance < 2*time.Second || c.STT.MaxUtterance > 29*time.Second {
		add("%s: %v is outside 2s to 29s", KeySTTMaxUtterance, c.STT.MaxUtterance)
	}

	if c.Mode == ModeAgent && c.LLM.APIKey == "" {
		add("%s is required in agent mode (create a key in the Anthropic Console)", KeyAnthropicAPIKey)
	}
	// Under 256 tokens a reply with a little thinking can be cut off; over
	// 8192 the ceiling no longer bounds anything a phone reply needs.
	if c.LLM.MaxTokens < 256 || c.LLM.MaxTokens > 8192 {
		add("%s: %d is outside 256 to 8192", KeyLLMMaxTokens, c.LLM.MaxTokens)
	}
	if c.MaxCallTime < 30*time.Second || c.MaxCallTime > time.Hour {
		add("%s: %v is outside 30s to 1h", KeyMaxCallTime, c.MaxCallTime)
	}
	if c.MaxTurns < 1 || c.MaxTurns > 100 {
		add("%s: %d is outside 1 to 100", KeyMaxTurns, c.MaxTurns)
	}

	// Outside this range the VAD either hears speech in line noise or misses
	// quiet callers altogether.
	if c.VAD.Threshold < 0.1 || c.VAD.Threshold > 0.95 {
		add("%s: %v is outside 0.1 to 0.95", KeyVADThreshold, c.VAD.Threshold)
	}
	// Under 200 ms, a breath between two words ends the utterance; over 3 s,
	// the caller waits too long for every answer.
	if c.VAD.Silence < 200*time.Millisecond || c.VAD.Silence > 3*time.Second {
		add("%s: %v is outside 200ms to 3s", KeyVADSilence, c.VAD.Silence)
	}
	return errs
}

func validPort(p int) bool { return p > 0 && p <= 65535 }

// Summary returns one log attribute per setting, as "value [source]". It is
// the first thing to read when a setting does not seem to take effect. The
// password appears only as its Fingerprint.
func (c *Config) Summary() []slog.Attr {
	attrs := make([]slog.Attr, 0, len(c.values))
	for _, key := range sortedKeys(c.values) {
		value := c.values[key]
		if secretKeys[key] {
			value = Fingerprint(value)
		}
		attrs = append(attrs, slog.String(key, fmt.Sprintf("%s [%s]", value, c.sources[key])))
	}
	return attrs
}

// unknownKeys reports PORTUNUS_ variables that this version does not use.
func unknownKeys(in Inputs) []string {
	known := make(map[string]bool, len(knownKeys))
	for _, k := range knownKeys {
		known[k] = true
	}

	var warnings []string
	report := func(m map[string]string, where Source) {
		for _, key := range sortedKeys(m) {
			if strings.HasPrefix(key, keyPrefix) && !known[key] {
				warnings = append(warnings, fmt.Sprintf("unknown setting %s in %s is ignored", key, where))
			}
		}
	}
	report(in.DotEnv, SourceDotEnv)
	report(in.Env, SourceEnv)
	return warnings
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loader resolves one setting at a time and collects every parse error, so a
// broken file reports all of its problems in one run instead of one per run.
type loader struct {
	in      Inputs
	values  map[string]string
	sources map[string]Source
	errs    []error
}

// lookup applies the precedence rule. A key that is present but empty still
// wins over the sources below it, and then means "use the default": an empty
// line in .env must not let a shell export leak through.
func (l *loader) lookup(key string) (string, Source) {
	if v, ok := l.in.Flags[key]; ok {
		return strings.TrimSpace(v), SourceFlag
	}
	if v, ok := l.in.DotEnv[key]; ok {
		return strings.TrimSpace(v), SourceDotEnv
	}
	if v, ok := l.in.Env[key]; ok {
		return strings.TrimSpace(v), SourceEnv
	}
	return "", SourceDefault
}

// raw returns the configured value, or "" with ok=false when the default
// applies. It also records the value and source for Summary.
func (l *loader) raw(key, def string) (string, bool) {
	v, src := l.lookup(key)
	l.sources[key] = src
	if v == "" {
		l.values[key] = def
		return "", false
	}
	l.values[key] = v
	return v, true
}

func (l *loader) fail(key, v string, err error) {
	l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, v, err))
}

func (l *loader) string(key, def string) string {
	if v, ok := l.raw(key, def); ok {
		return v
	}
	return def
}

func (l *loader) int(key string, def int) int {
	v, ok := l.raw(key, strconv.Itoa(def))
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, v, errors.New("not an integer"))
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := l.raw(key, strconv.FormatFloat(def, 'f', -1, 64))
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail(key, v, errors.New("not a number (for example 0.5)"))
		return def
	}
	return f
}

func (l *loader) bool(key string, def bool) bool {
	v, ok := l.raw(key, strconv.FormatBool(def))
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, v, errors.New("not a boolean (want true or false)"))
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key, def.String())
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, v, errors.New("not a duration (for example 0s, 500ms, 2s)"))
		return def
	}
	return d
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key, strings.ToLower(def.String()))
	if !ok {
		return def
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, v, errors.New("not a log level (want debug, info, warn or error)"))
		return def
	}
	return level
}

// hostPort parses "host" or "host:port". The port defaults to 5060.
func (l *loader) hostPort(key string) (string, int) {
	v, ok := l.raw(key, "")
	if !ok {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(v)
	if err != nil {
		// No port given. A bracketed IPv6 literal without a port lands here too.
		return strings.Trim(v, "[]"), defaultSIPPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || !validPort(port) {
		l.fail(key, v, errors.New("invalid port"))
		return host, 0
	}
	return host, port
}

// addr parses an optional IP address. The wildcard is rejected here, because
// SIP writes the address into Contact and SDP: a wildcard bind produces a
// Contact the PBX cannot reach.
func (l *loader) addr(key string) netip.Addr {
	v, ok := l.raw(key, "auto")
	if !ok {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		l.fail(key, v, errors.New("not an IP address (leave it empty to detect it)"))
		return netip.Addr{}
	}
	if a.IsUnspecified() || a.IsLoopback() {
		l.fail(key, v, errors.New("must be the LAN address the PBX can reach, not a wildcard or loopback address"))
		return netip.Addr{}
	}
	return a.Unmap()
}
