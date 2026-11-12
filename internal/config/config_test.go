package config

import (
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// minimal holds the settings Load requires in every mode, agent included.
// Tests copy it and change one thing.
func minimal() map[string]string {
	return map[string]string{
		KeySIPRegistrar:    "192.0.2.10",
		KeySIPExtension:    "200",
		KeySIPPassword:     "s3cret",
		KeyAnthropicAPIKey: "sk-test-key",
	}
}

func mustLoad(t *testing.T, in Inputs) *Config {
	t.Helper()
	cfg, _, err := Load(in)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	cfg := mustLoad(t, Inputs{DotEnv: minimal()})

	if cfg.Mode != ModeEcho {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeEcho)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.SIP.RegistrarHost != "192.0.2.10" || cfg.SIP.RegistrarPort != 5060 {
		t.Errorf("registrar = %s:%d, want 192.0.2.10:5060", cfg.SIP.RegistrarHost, cfg.SIP.RegistrarPort)
	}
	if cfg.SIP.AuthUser != "200" {
		t.Errorf("AuthUser = %q, want it to default to the extension %q", cfg.SIP.AuthUser, "200")
	}
	if cfg.SIP.BindAddr.IsValid() {
		t.Errorf("BindAddr = %v, want the zero value (auto)", cfg.SIP.BindAddr)
	}
	if cfg.SIP.Port != 5060 {
		t.Errorf("SIP.Port = %d, want 5060", cfg.SIP.Port)
	}
	if cfg.RTPPortMin != 20000 || cfg.RTPPortMax != 20099 {
		t.Errorf("RTP range = %d-%d, want 20000-20099", cfg.RTPPortMin, cfg.RTPPortMax)
	}
	if cfg.MaxConcurrentCalls != 2 {
		t.Errorf("MaxConcurrentCalls = %d, want 2", cfg.MaxConcurrentCalls)
	}
	if cfg.RingDelay != 0 {
		t.Errorf("RingDelay = %v, want 0", cfg.RingDelay)
	}
	if cfg.AllowPublicRegistrar {
		t.Error("AllowPublicRegistrar = true, want false")
	}
	if want := (TTS{URL: "http://127.0.0.1:8880", Model: "kokoro", Voice: "if_sara"}); cfg.TTS != want {
		t.Errorf("TTS = %+v, want %+v", cfg.TTS, want)
	}
	if cfg.OutputBuffer != 10*time.Second {
		t.Errorf("OutputBuffer = %v, want 10s", cfg.OutputBuffer)
	}
	if cfg.GreetingFile != "prompts/greeting.txt" {
		t.Errorf("GreetingFile = %q, want prompts/greeting.txt", cfg.GreetingFile)
	}
	if want := (STT{URL: "http://127.0.0.1:8890", Model: "large-v3-turbo", MaxUtterance: 9500 * time.Millisecond}); cfg.STT != want {
		t.Errorf("STT = %+v, want %+v", cfg.STT, want)
	}
	if want := (VAD{Threshold: 0.5, Silence: 700 * time.Millisecond}); cfg.VAD != want {
		t.Errorf("VAD = %+v, want %+v", cfg.VAD, want)
	}
	if want := (LLM{APIKey: "sk-test-key", Model: "claude-haiku-5-5", MaxTokens: 1024}); cfg.LLM != want {
		t.Errorf("LLM = %+v, want %+v", cfg.LLM, want)
	}
	if cfg.MaxCallTime != 5*time.Minute || cfg.MaxTurns != 20 {
		t.Errorf("ceilings = %v and %d turns, want 5m and 20", cfg.MaxCallTime, cfg.MaxTurns)
	}
	if cfg.SystemPromptFile != "prompts/system.txt" || cfg.AgentGreetingFile != "prompts/agent-greeting.txt" || cfg.HoldingFile != "prompts/holding.txt" {
		t.Errorf("agent files = %q, %q, %q", cfg.SystemPromptFile, cfg.AgentGreetingFile, cfg.HoldingFile)
	}
}

func TestLoadParsesValues(t *testing.T) {
	env := minimal()
	env[KeyLogLevel] = "DEBUG"
	env[KeySIPRegistrar] = "192.0.2.10:5080"
	env[KeySIPAuthUser] = "portunus-auth"
	env[KeySIPBindAddr] = "192.0.2.50"
	env[KeySIPPort] = "5070"
	env[KeyRTPPortMin] = "30000"
	env[KeyRTPPortMax] = "30009"
	env[KeyMaxConcurrentCalls] = "5"
	env[KeyRingDelay] = "1500ms"
	env[KeyAllowPublicRegistrar] = "true"
	env[KeyVADThreshold] = "0.65"
	env[KeyVADSilence] = "900ms"

	cfg := mustLoad(t, Inputs{DotEnv: env})

	if cfg.VAD.Threshold != 0.65 || cfg.VAD.Silence != 900*time.Millisecond {
		t.Errorf("VAD = %+v, want 0.65 and 900ms", cfg.VAD)
	}

	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.SIP.RegistrarPort != 5080 {
		t.Errorf("RegistrarPort = %d, want 5080", cfg.SIP.RegistrarPort)
	}
	if cfg.SIP.AuthUser != "portunus-auth" {
		t.Errorf("AuthUser = %q, want portunus-auth", cfg.SIP.AuthUser)
	}
	if cfg.SIP.BindAddr != netip.MustParseAddr("192.0.2.50") {
		t.Errorf("BindAddr = %v, want 192.0.2.50", cfg.SIP.BindAddr)
	}
	if cfg.SIP.Port != 5070 || cfg.RTPPortMin != 30000 || cfg.RTPPortMax != 30009 {
		t.Errorf("ports = %d, %d-%d", cfg.SIP.Port, cfg.RTPPortMin, cfg.RTPPortMax)
	}
	if cfg.MaxConcurrentCalls != 5 || cfg.RingDelay != 1500*time.Millisecond || !cfg.AllowPublicRegistrar {
		t.Errorf("calls=%d delay=%v public=%v", cfg.MaxConcurrentCalls, cfg.RingDelay, cfg.AllowPublicRegistrar)
	}
}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		in         Inputs
		wantMode   Mode
		wantSource Source
	}{
		{
			name:       "default when nothing is set",
			in:         Inputs{DotEnv: minimal()},
			wantMode:   ModeEcho,
			wantSource: SourceDefault,
		},
		{
			name:       "env when only the shell sets it",
			in:         Inputs{DotEnv: minimal(), Env: map[string]string{KeyMode: "listen"}},
			wantMode:   ModeListen,
			wantSource: SourceEnv,
		},
		{
			name: ".env beats the shell",
			in: Inputs{
				DotEnv: with(minimal(), KeyMode, "agent"),
				Env:    map[string]string{KeyMode: "listen"},
			},
			wantMode:   ModeAgent,
			wantSource: SourceDotEnv,
		},
		{
			name: "flag beats everything",
			in: Inputs{
				Flags:  map[string]string{KeyMode: "echo"},
				DotEnv: with(minimal(), KeyMode, "agent"),
				Env:    map[string]string{KeyMode: "listen"},
			},
			wantMode:   ModeEcho,
			wantSource: SourceFlag,
		},
		{
			// An empty line in .env must not let a stale shell export through.
			name: "empty .env value still beats the shell and means default",
			in: Inputs{
				DotEnv: with(minimal(), KeyMode, ""),
				Env:    map[string]string{KeyMode: "agent"},
			},
			wantMode:   ModeEcho,
			wantSource: SourceDotEnv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoad(t, tt.in)
			if cfg.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", cfg.Mode, tt.wantMode)
			}
			if got := cfg.sources[KeyMode]; got != tt.wantSource {
				t.Errorf("source = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want string // substring of the error
	}{
		{"missing registrar", KeySIPRegistrar, "", KeySIPRegistrar + " is required"},
		{"missing extension", KeySIPExtension, "", KeySIPExtension + " is required"},
		{"missing password", KeySIPPassword, "", KeySIPPassword + " is required"},
		{"bad extension", KeySIPExtension, "200@pbx", "not a valid SIP user"},
		{"unknown mode", KeyMode, "party", `unknown mode "party"`},
		{"bad log level", KeyLogLevel, "loud", "not a log level"},
		{"bad registrar port", KeySIPRegistrar, "192.0.2.10:abc", "invalid port"},
		{"bad sip port", KeySIPPort, "70000", "not a valid port"},
		{"non-numeric port", KeySIPPort, "five", "not an integer"},
		{"wildcard bind", KeySIPBindAddr, "0.0.0.0", "not a wildcard or loopback"},
		{"loopback bind", KeySIPBindAddr, "127.0.0.1", "not a wildcard or loopback"},
		{"hostname bind", KeySIPBindAddr, "portunus.local", "not an IP address"},
		{"inverted rtp range", KeyRTPPortMax, "19000", "not a valid port range"},
		{"zero calls", KeyMaxConcurrentCalls, "0", "must be at least 1"},
		{"negative ring delay", KeyRingDelay, "-1s", "must not be negative"},
		{"bad ring delay", KeyRingDelay, "2", "not a duration"},
		{"bad bool", KeyAllowPublicRegistrar, "yes", "not a boolean"},
		{"tts url without scheme", KeyTTSURL, "127.0.0.1:8880", "not an http(s) URL"},
		{"tts url with another scheme", KeyTTSURL, "ws://127.0.0.1:8880", "not an http(s) URL"},
		{"output buffer too small", KeyOutputBuffer, "500ms", "must be at least 1s"},
		{"stt url without scheme", KeySTTURL, "127.0.0.1:8890", "not an http(s) URL"},
		{"stt max utterance too short", KeySTTMaxUtterance, "1s", "outside 2s to 29s"},
		{"stt max utterance too long", KeySTTMaxUtterance, "30s", "outside 2s to 29s"},
		{"llm max tokens too few", KeyLLMMaxTokens, "100", "outside 256 to 8192"},
		{"max call time too short", KeyMaxCallTime, "10s", "outside 30s to 1h"},
		{"max turns zero", KeyMaxTurns, "0", "outside 1 to 100"},
		{"thinking not a boolean", KeyLLMThinking, "maybe", "not a boolean"},
		{"vad threshold not a number", KeyVADThreshold, "half", "not a number"},
		{"vad threshold too low", KeyVADThreshold, "0.05", "outside 0.1 to 0.95"},
		{"vad threshold too high", KeyVADThreshold, "1", "outside 0.1 to 0.95"},
		{"vad silence too short", KeyVADSilence, "100ms", "outside 200ms to 3s"},
		{"vad silence too long", KeyVADSilence, "5s", "outside 200ms to 3s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(Inputs{DotEnv: with(minimal(), tt.key, tt.val)})
			if err == nil {
				t.Fatalf("Load with %s=%q: expected an error", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadRTPRangeMustFitTheCalls(t *testing.T) {
	// 20000-20005 holds three calls: RTP on 20000, 20002, 20004 and RTCP one above.
	env := with(minimal(), KeyRTPPortMax, "20005")
	env[KeyMaxConcurrentCalls] = "3"
	if _, _, err := Load(Inputs{DotEnv: env}); err != nil {
		t.Errorf("three calls in six ports: unexpected error %v", err)
	}

	env[KeyMaxConcurrentCalls] = "4"
	_, _, err := Load(Inputs{DotEnv: env})
	if err == nil || !strings.Contains(err.Error(), "fits 3 calls") {
		t.Errorf("four calls in six ports: error = %v, want one saying the range fits 3 calls", err)
	}
}

func TestLoadReportsEveryError(t *testing.T) {
	_, _, err := Load(Inputs{DotEnv: map[string]string{KeySIPPort: "x"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{KeySIPRegistrar, KeySIPExtension, KeySIPPassword, KeySIPPort} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadWarnsAboutUnknownKeys(t *testing.T) {
	_, warnings, err := Load(Inputs{
		DotEnv: with(minimal(), "PORTUNUS_SIP_PASWORD", "typo"),
		Env: map[string]string{
			KeyEnvFile:       "/etc/portunus.env", // known, read by cmd/portunus
			"PORTUNUS_COLOR": "blue",
			"HOME":           "/home/someone", // not ours: no warning
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{
		"unknown setting PORTUNUS_SIP_PASWORD in .env is ignored",
		"unknown setting PORTUNUS_COLOR in env is ignored",
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
}

func TestSummaryHidesThePassword(t *testing.T) {
	cfg := mustLoad(t, Inputs{DotEnv: minimal()})

	var password, registrar string
	for _, a := range cfg.Summary() {
		if strings.Contains(a.Value.String(), "s3cret") || strings.Contains(a.Value.String(), "sk-test-key") {
			t.Errorf("summary attribute %s leaks a secret: %q", a.Key, a.Value)
		}
		switch a.Key {
		case KeySIPPassword:
			password = a.Value.String()
		case KeySIPRegistrar:
			registrar = a.Value.String()
		case KeyAnthropicAPIKey:
			if want := Fingerprint("sk-test-key") + " [.env]"; a.Value.String() != want {
				t.Errorf("API key summary = %q, want %q", a.Value, want)
			}
		}
	}
	if want := Fingerprint("s3cret") + " [.env]"; password != want {
		t.Errorf("password summary = %q, want %q", password, want)
	}
	if want := "192.0.2.10 [.env]"; registrar != want {
		t.Errorf("registrar summary = %q, want %q", registrar, want)
	}
}

func TestFingerprint(t *testing.T) {
	if got := Fingerprint(""); got != "(empty)" {
		t.Errorf("Fingerprint(\"\") = %q, want (empty)", got)
	}
	// Fixed vector: printf '%s' s3cret | shasum -a 256 | cut -c1-8
	if got, want := Fingerprint("s3cret"), "sha256:1ec1c26b"; got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Error("different secrets produced the same fingerprint")
	}
}

func with(m map[string]string, key, value string) map[string]string {
	m[key] = value
	return m
}

func TestAgentModeNeedsTheAPIKey(t *testing.T) {
	env := with(minimal(), KeyMode, "agent")
	delete(env, KeyAnthropicAPIKey)
	if _, _, err := Load(Inputs{DotEnv: env}); err == nil || !strings.Contains(err.Error(), KeyAnthropicAPIKey) {
		t.Errorf("agent mode without a key: error = %v, want one naming %s", err, KeyAnthropicAPIKey)
	}
	// The other modes do not talk to Claude, so they do not need it.
	env[KeyMode] = "listen"
	if _, _, err := Load(Inputs{DotEnv: env}); err != nil {
		t.Errorf("listen mode without a key: %v", err)
	}
}

func TestTimeZone(t *testing.T) {
	if got := mustLoad(t, Inputs{Env: minimal()}).TimeZone.String(); got != "Europe/Rome" {
		t.Errorf("default time zone %q, want Europe/Rome", got)
	}

	in := minimal()
	in[KeyTimeZone] = "UTC"
	if got := mustLoad(t, Inputs{Env: in}).TimeZone.String(); got != "UTC" {
		t.Errorf("time zone %q, want UTC", got)
	}

	in[KeyTimeZone] = "Europe/Roma"
	if _, _, err := Load(Inputs{Env: in}); err == nil || !strings.Contains(err.Error(), KeyTimeZone) {
		t.Errorf("a misspelled zone must be an error naming %s, got %v", KeyTimeZone, err)
	}
}

// TestEnvExampleMatchesTheCode keeps .env.example honest: it must parse, it
// must hold no setting this version ignores, and it must list every setting
// there is, so it never falls behind the code.
func TestEnvExampleMatchesTheCode(t *testing.T) {
	example, err := ReadDotEnv("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	if warnings := unknownKeys(Inputs{DotEnv: example}); len(warnings) > 0 {
		t.Errorf(".env.example holds settings the code does not know: %v", warnings)
	}
	for _, key := range append(knownKeys, KeyAnthropicAPIKey) {
		if key == KeyEnvFile {
			continue // read from the environment only, before any file
		}
		if _, ok := example[key]; !ok {
			t.Errorf(".env.example lacks %s", key)
		}
	}
}
