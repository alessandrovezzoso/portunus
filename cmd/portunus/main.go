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
	"strings"
	"syscall"

	"github.com/alessandrovezzoso/portunus/internal/config"
	"github.com/alessandrovezzoso/portunus/internal/sip"
)

// version is set at build time by the Makefile (-ldflags "-X main.version=...").
var version = "dev"

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

	if cfg.Mode != config.ModeEcho {
		return fmt.Errorf("mode %q is not implemented yet: this milestone only has echo", cfg.Mode)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore the default handling: if the graceful
	// shutdown ever hangs, a second Ctrl+C still kills the process.
	context.AfterFunc(ctx, stop)

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
		Logger:             log,
	})
	if err != nil {
		return err
	}

	err = srv.Run(ctx, sip.Echo)
	log.Info("stopped")
	return err
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
