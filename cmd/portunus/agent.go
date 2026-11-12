package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/call"
	"github.com/alessandrovezzoso/portunus/internal/config"
	"github.com/alessandrovezzoso/portunus/internal/llm/anthropic"
	"github.com/alessandrovezzoso/portunus/internal/sip"
	sttapi "github.com/alessandrovezzoso/portunus/internal/stt/openaicompat"
	ttsapi "github.com/alessandrovezzoso/portunus/internal/tts/openaicompat"
)

const (
	// llmCheckTimeout bounds the startup check of the Anthropic key.
	llmCheckTimeout = 10 * time.Second
	// holdingTimeout bounds the synthesis of the holding phrases at startup.
	holdingTimeout = time.Minute
)

// agentHandler builds the agent mode: the texts, every adapter, the startup
// checks, and the readiness check run before each call.
func agentHandler(ctx context.Context, cfg *config.Config, log *slog.Logger) (sip.Handler, func(context.Context) error, error) {
	systemPrompt, err := readText(config.KeySystemPromptFile, cfg.SystemPromptFile)
	if err != nil {
		return nil, nil, err
	}
	greeting, err := readText(config.KeyAgentGreetingFile, cfg.AgentGreetingFile)
	if err != nil {
		return nil, nil, err
	}
	phrases, err := readHolding(cfg.HoldingFile, log)
	if err != nil {
		return nil, nil, err
	}
	log.Info("agent texts loaded",
		"system_prompt_characters", len([]rune(systemPrompt)),
		"greeting_characters", len([]rune(greeting)),
		"holding_phrases", len(phrases))

	llm, err := anthropic.New(anthropic.Options{
		APIKey:       cfg.LLM.APIKey,
		Model:        cfg.LLM.Model,
		MaxTokens:    cfg.LLM.MaxTokens,
		SystemPrompt: systemPrompt,
		Thinking:     cfg.LLM.Thinking,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := preflightLLM(ctx, llm, cfg.LLM.Model, log); err != nil {
		return nil, nil, err
	}

	tts, err := ttsapi.New(ttsapi.Options{BaseURL: cfg.TTS.URL, Model: cfg.TTS.Model, Voice: cfg.TTS.Voice})
	if err != nil {
		return nil, nil, err
	}
	preflightService(ctx, speechService{
		name: "TTS", url: cfg.TTS.URL, check: tts.Check, timeout: ttsCheckTimeout,
		leaves: "the text sent to it", start: "make tts-up",
	}, log)

	stt, err := sttapi.New(sttapi.Options{BaseURL: cfg.STT.URL, Model: cfg.STT.Model, Language: callerLanguage})
	if err != nil {
		return nil, nil, err
	}
	preflightService(ctx, speechService{
		name: "STT", url: cfg.STT.URL, check: stt.Check, timeout: sttCheckTimeout,
		leaves: "the callers' voice", start: "make stt-up",
	}, log)

	vad, err := newVAD()
	if err != nil {
		return nil, nil, err
	}

	// The holding phrases are synthesized once, here. Without Kokoro they
	// cannot be, and calls go on without them: they are a comfort, not a
	// requirement.
	hctx, cancel := context.WithTimeout(ctx, holdingTimeout)
	holding, err := call.PrepareHolding(hctx, tts, phrases)
	cancel()
	if err != nil {
		log.Warn("holding phrases unavailable; calls will have silence instead", "error", err)
		holding = nil
	}

	gate := &readiness{
		tts:   ttsPinger{check: tts.Check},
		stt:   stt,
		probe: llm.Probe,
		log:   log,
	}
	handler := call.Agent(claudeLLM{llm}, stt, tts, vad, call.AgentOptions{
		Greeting:      greeting,
		Holding:       holding,
		MaxCallTime:   cfg.MaxCallTime,
		MaxTurns:      cfg.MaxTurns,
		OutputBuffer:  cfg.OutputBuffer,
		VADThreshold:  float32(cfg.VAD.Threshold),
		VADSilence:    cfg.VAD.Silence,
		MaxUtterance:  cfg.STT.MaxUtterance,
		OnUnavailable: gate.markDown,
		Logger:        log,
	})
	return handler, gate.ready, nil
}

// preflightLLM checks the Anthropic key and model before registering. A
// rejected key or an unknown model is fatal, as CLAUDE.md asks: Portunus then
// never registers, and the S20 sends calls to its fallback instead of to a
// dead line. An unreachable API is only a warning.
func preflightLLM(ctx context.Context, llm *anthropic.Client, model string, log *slog.Logger) error {
	cctx, cancel := context.WithTimeout(ctx, llmCheckTimeout)
	defer cancel()
	switch err := llm.Check(cctx); {
	case err == nil:
		log.Info("Anthropic API ready", "model", model)
	case anthropic.Fatal(err):
		return fmt.Errorf("the Anthropic API rejected the key or the model %q (check %s and %s): %w",
			model, config.KeyAnthropicAPIKey, config.KeyLLMModel, err)
	default:
		log.Warn("Anthropic API not reachable; calls will fail until it is", "error", err)
	}
	return nil
}

// readHolding reads the holding phrases, one per line. The file is optional:
// without it, the defaults apply.
func readHolding(path string, log *slog.Logger) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		log.Info("no holding phrase file; using the defaults", "file", path)
		return call.DefaultHoldingPhrases, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.KeyHoldingFile, err)
	}
	var phrases []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			phrases = append(phrases, line)
		}
	}
	if len(phrases) == 0 {
		return nil, fmt.Errorf("%s: %s has no phrases (delete it to use the defaults)", config.KeyHoldingFile, path)
	}
	return phrases, nil
}
