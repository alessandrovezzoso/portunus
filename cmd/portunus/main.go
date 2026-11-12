// Command portunus is an AI answering service for a SIP PBX. It registers with
// the PBX as one ordinary extension and answers the calls routed to it.
//
// This file only wires things together: it reads the configuration, runs the
// preflight checks and starts the SIP server with the handler for the
// selected mode. The packages it wires never import each other's config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/call"
	"github.com/alessandrovezzoso/portunus/internal/config"
	"github.com/alessandrovezzoso/portunus/internal/sip"
	sttapi "github.com/alessandrovezzoso/portunus/internal/stt/openaicompat"
	ttsapi "github.com/alessandrovezzoso/portunus/internal/tts/openaicompat"
	"github.com/alessandrovezzoso/portunus/internal/vad/silero"
)

// version is set at build time by the Makefile (-ldflags "-X main.version=...").
var version = "dev"

// callerLanguage is what callers speak. CLAUDE.md fixes it: every adapter and
// prompt is judged on Italian, so it is not a setting.
const callerLanguage = "it"

// The preflight checks of the speech servers. The STT check runs a real
// transcription, which on a CPU takes seconds, and the first one after the
// server starts is the slowest.
const (
	ttsCheckTimeout = 5 * time.Second
	sttCheckTimeout = time.Minute
)

func main() {
	if err := run(); err != nil {
		// Plain stderr rather than slog: the error may come from loading the
		// very config that sets up the logger.
		fmt.Fprintln(os.Stderr, "portunus:", err)
		os.Exit(1)
	}
}

func run() error {
	// -mode exists so a diagnostic mode cannot be overridden by whatever the
	// .env file says: flags beat everything.
	mode := flag.String("mode", "", "run in this mode, overriding "+config.KeyMode+": echo, greet, listen or agent")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %q", flag.Args())
	}

	dotenv, envFile, err := readEnvFile()
	if err != nil {
		return err
	}
	flags := map[string]string{}
	if *mode != "" {
		flags[config.KeyMode] = *mode
	}
	cfg, warnings, err := config.Load(config.Inputs{Flags: flags, DotEnv: dotenv, Env: environ()})
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	log.Info("starting portunus", "version", version, "mode", cfg.Mode, "env_file", envFile)
	log.LogAttrs(context.Background(), slog.LevelInfo, "effective config", cfg.Summary()...)
	for _, w := range warnings {
		log.Warn(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore the default handling: if the graceful
	// shutdown ever hangs, a second Ctrl+C still kills the process.
	context.AfterFunc(ctx, stop)

	handler, ready, err := handlerFor(ctx, cfg, log)
	if err != nil {
		return err
	}

	bindAddr, err := preflight(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	srv, err := sip.NewServer(sip.Options{
		BindAddr:           bindAddr,
		Port:               cfg.SIP.Port,
		RegistrarHost:      cfg.SIP.RegistrarHost,
		RegistrarPort:      cfg.SIP.RegistrarPort,
		Extension:          cfg.SIP.Extension,
		AuthUser:           cfg.SIP.AuthUser,
		Password:           cfg.SIP.Password,
		UserAgent:          "Portunus/" + version,
		RTPPortMin:         cfg.RTPPortMin,
		RTPPortMax:         cfg.RTPPortMax,
		MaxConcurrentCalls: cfg.MaxConcurrentCalls,
		RingDelay:          cfg.RingDelay,
		Ready:              ready,
		Logger:             log,
	})
	if err != nil {
		return err
	}

	err = srv.Run(ctx, handler)
	log.Info("stopped")
	return err
}

// handlerFor picks what Portunus does with an answered call, and builds the
// adapters that mode needs. This is the one place that knows both the config
// and the concrete adapters.
//
// It also returns the check run before answering each call, or nil. Only the
// agent mode has one: the diagnostic modes answer whatever happens, because
// seeing the failure is their point.
func handlerFor(ctx context.Context, cfg *config.Config, log *slog.Logger) (sip.Handler, func(context.Context) error, error) {
	switch cfg.Mode {
	case config.ModeEcho:
		return sip.Echo, nil, nil

	case config.ModeAgent:
		return agentHandler(ctx, cfg, log)

	case config.ModeGreet:
		greeting, err := readGreeting(cfg.GreetingFile)
		if err != nil {
			return nil, nil, err
		}
		log.Info("greeting loaded", "file", cfg.GreetingFile, "characters", len([]rune(greeting)))

		tts, err := ttsapi.New(ttsapi.Options{
			BaseURL: cfg.TTS.URL,
			Model:   cfg.TTS.Model,
			Voice:   cfg.TTS.Voice,
		})
		if err != nil {
			return nil, nil, err
		}
		preflightService(ctx, speechService{
			name: "TTS", url: cfg.TTS.URL, check: tts.Check, timeout: ttsCheckTimeout,
			leaves: "the text sent to it", start: "make tts-up",
		}, log)

		return call.Greet(tts, greeting, call.GreetOptions{
			OutputBuffer: cfg.OutputBuffer,
			Logger:       log,
		}), nil, nil

	case config.ModeListen:
		stt, err := sttapi.New(sttapi.Options{
			BaseURL:  cfg.STT.URL,
			Model:    cfg.STT.Model,
			Language: callerLanguage,
		})
		if err != nil {
			return nil, nil, err
		}
		preflightService(ctx, speechService{
			name: "STT", url: cfg.STT.URL, check: stt.Check, timeout: sttCheckTimeout,
			leaves: "the callers' voice", start: "make stt-up",
		}, log)

		// The VAD runs in this process with embedded weights, so failing to
		// load it means a broken binary: that is fatal, unlike a server that
		// may still be starting.
		vad, err := newVAD()
		if err != nil {
			return nil, nil, err
		}

		return call.Listen(stt, vad, call.ListenOptions{
			VADThreshold: float32(cfg.VAD.Threshold),
			VADSilence:   cfg.VAD.Silence,
			MaxUtterance: cfg.STT.MaxUtterance,
			Logger:       log,
		}), nil, nil
	}
	return nil, nil, fmt.Errorf("mode %q is not known", cfg.Mode)
}

// newVAD checks that the VAD loads, and returns the function that makes one
// per call. It runs in this process with embedded weights, so failing to load
// it means a broken binary: that is fatal, unlike a server that may still be
// starting.
func newVAD() (func() (call.VAD, error), error) {
	if _, err := silero.New(); err != nil {
		return nil, err
	}
	return func() (call.VAD, error) { return silero.New() }, nil
}

// readGreeting reads the greet-mode text.
func readGreeting(path string) (string, error) {
	return readText(config.KeyGreetingFile, path)
}

// readText reads one of the texts in prompts/. A missing or empty file is
// fatal: a greeting or a prompt that says nothing proves nothing. The error
// points at the example file to start from.
func readText(key, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		example := filepath.Join(filepath.Dir(path), "example."+filepath.Base(path))
		return "", fmt.Errorf("%s: %s does not exist (copy %s to start)", key, path, example)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", fmt.Errorf("%s: %s is empty", key, path)
	}
	return text, nil
}

// readEnvFile reads PORTUNUS_ENV_FILE, or ./.env when that is unset. A missing
// default file is fine (under systemd, EnvironmentFile fills the environment
// instead), but a file named explicitly must exist: a typo in the path must
// not silently start Portunus with no config.
func readEnvFile() (values map[string]string, path string, err error) {
	path, explicit := os.LookupEnv(config.KeyEnvFile)
	if !explicit || path == "" {
		path, explicit = ".env", false
	}

	values, err = config.ReadDotEnv(path)
	switch {
	case err == nil:
		return values, path, nil
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		return nil, "(none)", nil
	default:
		return nil, "", fmt.Errorf("read env file: %w", err)
	}
}

// environ returns the process environment as a map.
func environ() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}
