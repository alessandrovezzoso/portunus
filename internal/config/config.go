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
}

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
		if key == KeySIPPassword {
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
