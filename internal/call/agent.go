package call

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/alessandrovezzoso/portunus/internal/media"
	"github.com/alessandrovezzoso/portunus/internal/sip"
	"github.com/alessandrovezzoso/portunus/internal/speech"
)

// AgentOptions configures the agent mode. Zero values mean the defaults,
// except Greeting, which is required.
type AgentOptions struct {
	// Greeting is said as soon as the call is answered. It is fixed text,
	// not generated: it is heard at once, and it always carries the notice
	// that the caller is talking to an automated assistant.
	Greeting string
	// Holding are clips from PrepareHolding, one played the moment the caller
	// stops talking and taking turns. Empty means no holding phrase.
	Holding [][]int16

	// The per-call cost ceilings. Either one ends the call gracefully, with a
	// closing line, never mid-sentence.
	MaxCallTime time.Duration
	MaxTurns    int

	OutputBuffer time.Duration
	VADThreshold float32
	VADSilence   time.Duration
	MaxUtterance time.Duration

	// OnUnavailable is told when the LLM reports a failure no call can get
	// past (credit exhausted, key revoked). cmd/portunus then refuses calls,
	// so the PBX sends them to voicemail.
	OnUnavailable func(error)

	Logger *slog.Logger

	// For tests: the time a hangup waits for the last words to be heard, how
	// long after the assistant falls silent the ear stays muted, and how far
	// past MaxCallTime the hard ceiling sits.
	tail      time.Duration
	muteTail  time.Duration
	hardGrace time.Duration
}

const (
	defaultMaxCallTime = 5 * time.Minute
	defaultMaxTurns    = 20
	// defaultHardGrace is how long a turn may run past MaxCallTime before
	// the call is cut. MaxCallTime itself is checked between turns, so the
	// closing line is never cut; this backstop only fires when a turn is
	// stuck (a speech server that stops answering mid-stream), and then a cut
	// is the only way out.
	defaultHardGrace = 30 * time.Second
	// echoTail keeps the ear muted briefly after the assistant stops: the
	// last syllable is still in the phone's jitter buffer, and its echo can
	// come back after that.
	echoTail = 300 * time.Millisecond
)

// The fixed lines. They are generic on purpose: nothing here names the
// office, and they are committed to a public repository.
const (
	lineNotUnderstood = "Mi scusi, non ho capito bene. Può ripetere, per favore?"
	lineTechnical     = "Mi scusi, ho un problema tecnico. La faremo richiamare al più presto. Arrivederci."
	lineRefusal       = "Mi scusi, su questo non posso aiutarla. Posso però lasciare un messaggio per l'ufficio."
	lineClosing       = "Mi scusi, devo chiudere la chiamata. La faremo richiamare al più presto. Arrivederci."
)

// How a conversation ended, for the log.
const (
	endAssistant      = "assistant"       // the model hung up after saying goodbye
	endCaller         = "caller"          // the caller hung up, or Portunus is shutting down
	endMaxTurns       = "max_turns"       // the turn ceiling
	endMaxCallTime    = "max_call_time"   // the time ceiling
	endLLMUnavailable = "llm_unavailable" // credit or key: no call can be served
	endLLMError       = "llm_error"       // the model failed for this call
	endTTSError       = "tts_error"       // Portunus could not speak
)

// Agent is the handler for the agent mode: the full conversation.
//
// A call goes greeting → listen → (holding phrase, STT, model, speech) →
// listen…, until the model hangs up, the caller does, or a ceiling ends it.
// Until barge-in exists the two sides take turns: what the caller says while
// the assistant is audible is not heard.
func Agent(llm LLM, stt STT, tts TTS, newVAD func() (VAD, error), opts AgentOptions) sip.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxCallTime <= 0 {
		opts.MaxCallTime = defaultMaxCallTime
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = defaultMaxTurns
	}
	if opts.tail <= 0 {
		opts.tail = defaultTail
	}
	if opts.muteTail <= 0 {
		opts.muteTail = echoTail
	}
	if opts.hardGrace <= 0 {
		opts.hardGrace = defaultHardGrace
	}
	segCfg := segmenterConfigFor(opts.VADThreshold, opts.VADSilence, opts.MaxUtterance)
	// The TTS gets the greeting with its pause tags; the model gets the words.
	heardGreeting := speech.StripPauses(opts.Greeting)

	return func(ctx context.Context, c *sip.Call) error {
		answered := time.Now()
		log := log.With("call_id", c.ID)
		vad, err := newVAD()
		if err != nil {
			return fmt.Errorf("vad: %w", err)
		}

		// The hard ceiling bounds the call whatever is stuck inside it. The
		// cancel below still ends it earlier, at hangup or after the goodbye.
		ctx, cancelHard := context.WithTimeoutCause(ctx, opts.MaxCallTime+opts.hardGrace, errHardCeiling)
		defer cancelHard()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		law := lawFor(c.Codec)
		pacer := media.NewPacer(law.Silence(), int(opts.OutputBuffer.Seconds()*media.BytesPerSecond))
		paced := make(chan error, 1)
		go func() { paced <- pacer.Run(ctx, c.Outbound) }()

		a := &agentCall{
			opts: opts,
			stt:  stt,
			conv: llm.NewConversation(CallInfo{Start: answered, Greeting: heardGreeting}),
			m:    &mouth{tts: tts, law: law, pacer: pacer, log: log},
			log:  log,
			rate: vad.SampleRate(),
		}
		a.ear = newEar(vad, law, segCfg, a.muted, log)

		// The ear reads until the caller's audio ends: that is how a hangup
		// shows up, and it cancels whatever the call was doing.
		go func() {
			if err := a.ear.run(ctx, c.Inbound); err != nil {
				log.Warn("inbound audio failed", "error", err)
			}
			cancel()
		}()

		reason := a.converse(ctx, answered)

		// Let the last words be heard before hanging up, unless the caller
		// has already gone.
		if ctx.Err() == nil {
			waitDrained(ctx, pacer)
			sleepCtx(ctx, opts.tail)
		}
		cancel()
		// A hangup closes the media under the pacer: that write error is the
		// call ending. Anything else is worth a line.
		if err := <-paced; err != nil && !mediaClosed(err) {
			log.Warn("sending audio failed", "error", err)
		}

		a.logSummary(pacer.Stats(), reason)
		return nil
	}
}

// agentCall is the state of one conversation. Only the handler's goroutine
// touches it, except lastAudible, which only the ear's goroutine touches
// (through muted).
type agentCall struct {
	opts AgentOptions
	stt  STT
	conv Conversation
	m    *mouth
	ear  *ear
	log  *slog.Logger
	rate int // sample rate of the utterances, the VAD's

	lastAudible time.Time

	replies     int
	holdingNext int
	usage       Usage
	sttTimes    latencies
	firstAudio  latencies
}

// latencies sums up one per-turn measurement for the end-of-call log.
type latencies struct {
	n          int
	total, max time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.n++
	l.total += d
	l.max = max(l.max, d)
}

func (l latencies) attrs(name string) []any {
	if l.n == 0 {
		return nil
	}
	return []any{
		name + "_avg", (l.total / time.Duration(l.n)).Milliseconds(),
		name + "_max", l.max.Milliseconds(),
	}
}

// muted reports whether the ear should ignore what it hears: while the
// assistant is audible, and for a short echo tail after.
func (a *agentCall) muted() bool {
	if a.m.audible() {
		a.lastAudible = time.Now()
		return true
	}
	return time.Since(a.lastAudible) < a.opts.muteTail
}

// converse runs the conversation and returns how it ended.
func (a *agentCall) converse(ctx context.Context, answered time.Time) string {
	if err := a.m.say(ctx, a.opts.Greeting); err != nil {
		return a.speechFailed(ctx, err)
	}

	deadline := time.NewTimer(a.opts.MaxCallTime - time.Since(answered))
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return a.interrupted(ctx)
		case <-deadline.C:
			a.log.Info("call time ceiling reached", "max_call_time", a.opts.MaxCallTime)
			return a.close(ctx, lineClosing, endMaxCallTime)
		case q, ok := <-a.ear.queue:
			if !ok {
				return a.interrupted(ctx)
			}
			if reason := a.turn(ctx, q); reason != "" {
				return reason
			}
		}
	}
}

// turn answers one utterance. It returns an end reason when the call must
// end, and "" to keep listening.
func (a *agentCall) turn(ctx context.Context, q queued) string {
	a.playHolding()

	text, err := a.transcribe(ctx, q)
	sttDur := time.Since(q.ended)
	switch {
	case ctx.Err() != nil:
		return a.interrupted(ctx)
	case err != nil:
		a.log.Warn("transcription failed", "turn", q.turn, "error", err)
		if err := a.m.say(ctx, lineNotUnderstood); err != nil {
			return a.speechFailed(ctx, err)
		}
		return ""
	case text == "":
		// Nothing intelligible: a noise, or a word Whisper could not make
		// out. The holding phrase has already told the caller an answer is
		// coming, so silence now would leave them hanging: ask again.
		// Without a holding phrase nothing was said, and waiting is enough.
		if len(a.opts.Holding) == 0 {
			return ""
		}
		a.log.Info("nothing intelligible in the utterance", "turn", q.turn)
		if err := a.m.say(ctx, lineNotUnderstood); err != nil {
			return a.speechFailed(ctx, err)
		}
		return ""
	}
	a.sttTimes.add(sttDur)

	if a.replies >= a.opts.MaxTurns {
		a.log.Info("turn ceiling reached", "max_turns", a.opts.MaxTurns)
		return a.close(ctx, lineClosing, endMaxTurns)
	}

	// The reply is spoken while it streams: the first sentence goes to the
	// TTS while the model writes the next.
	llmStart := time.Now()
	var llmFirst time.Duration
	spoken := a.m.begin(ctx)
	reply, err := a.conv.Reply(ctx, text, func(delta string) {
		if llmFirst == 0 {
			llmFirst = time.Since(llmStart)
		}
		spoken.push(delta)
	})
	ttsFirst, onWire, speakErr := spoken.finish()
	// Counted before anything else: a reply cut by a hangup or an error was
	// billed all the same.
	a.usage.add(reply.Usage)

	switch {
	case ctx.Err() != nil:
		return a.interrupted(ctx)
	case err != nil && unavailable(err):
		a.log.Error("the model cannot serve calls: refusing calls until it can", "error", err)
		if a.opts.OnUnavailable != nil {
			a.opts.OnUnavailable(err)
		}
		return a.close(ctx, lineTechnical, endLLMUnavailable)
	case err != nil:
		a.log.Error("the model failed", "turn", q.turn, "error", err)
		return a.close(ctx, lineTechnical, endLLMError)
	}
	a.replies++

	attrs := []any{
		"turn", a.replies,
		"stt_ms", sttDur.Milliseconds(),
		"llm_first_token_ms", llmFirst.Milliseconds(),
	}
	if !onWire.IsZero() {
		a.firstAudio.add(onWire.Sub(q.ended))
		attrs = append(attrs,
			"tts_first_audio_ms", ttsFirst.Milliseconds(),
			"first_audio_ms", onWire.Sub(q.ended).Milliseconds(),
		)
	}
	a.log.Info("turn latency", attrs...)
	// What a third party said, and what the assistant answered: debug only.
	a.log.Debug("turn text", "turn", a.replies, "caller", text, "assistant", reply.Text)

	if speakErr != nil {
		return a.speechFailed(ctx, speakErr)
	}
	if reply.Refused {
		a.log.Warn("the model declined to answer", "turn", a.replies)
		if err := a.m.say(ctx, lineRefusal); err != nil {
			return a.speechFailed(ctx, err)
		}
	}
	if reply.EndCall {
		return endAssistant
	}
	return ""
}

// transcribe turns the utterance into text, together with any utterance
// queued behind it: what the caller added while the first one was being
// transcribed belongs to the same turn.
func (a *agentCall) transcribe(ctx context.Context, q queued) (string, error) {
	parts := []queued{q}
	for more := true; more; {
		select {
		case next, ok := <-a.ear.queue:
			if !ok {
				more = false
				break
			}
			parts = append(parts, next)
		default:
			more = false
		}
	}

	var texts []string
	for _, p := range parts {
		pcm := p.pcm
		if rate := a.stt.SampleRate(); rate != a.rate {
			pcm = resampleAll(pcm, a.rate, rate)
		}
		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		text, err := a.stt.Transcribe(rctx, pcm)
		cancel()
		if err != nil {
			return "", err
		}
		if text = strings.TrimSpace(text); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, " "), nil
}

// playHolding plays the next holding phrase, in rotation.
func (a *agentCall) playHolding() {
	if len(a.opts.Holding) == 0 {
		return
	}
	a.m.play(a.opts.Holding[a.holdingNext%len(a.opts.Holding)])
	a.holdingNext++
}

// close says a last line and reports reason. The handler hangs up once the
// line has been heard.
func (a *agentCall) close(ctx context.Context, line, reason string) string {
	if err := a.m.say(ctx, line); err != nil && ctx.Err() == nil {
		a.log.Error("closing line failed", "error", err)
	}
	return reason
}

// speechFailed ends a call that cannot be spoken to.
func (a *agentCall) speechFailed(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return a.interrupted(ctx)
	}
	a.log.Error("speech synthesis failed", "error", err)
	return endTTSError
}

// errHardCeiling is the cause of a call cut by the hard time ceiling.
var errHardCeiling = errors.New("call cut by the hard time ceiling")

// interrupted reports why the call's context ended: the hard ceiling, or the
// caller hanging up (Portunus shutting down counts as that too).
func (a *agentCall) interrupted(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errHardCeiling) {
		a.log.Warn("call cut: a turn was still running past the time ceiling",
			"max_call_time", a.opts.MaxCallTime, "grace", a.opts.hardGrace)
		return endMaxCallTime
	}
	return endCaller
}

// logSummary writes the end-of-call lines.
func (a *agentCall) logSummary(st media.PacerStats, reason string) {
	a.log.Info("call audio quality",
		"starved_pct", math.Round(st.StarvedPct()*10)/10,
		"dropped_bytes", st.DroppedBytes,
		"ignored_utterances", a.ear.ignored.Load(),
		"dropped_utterances", a.ear.dropped.Load(),
	)
	attrs := []any{"turns", a.replies}
	attrs = append(attrs, a.sttTimes.attrs("stt_ms")...)
	attrs = append(attrs, a.firstAudio.attrs("first_audio_ms")...)
	a.log.Info("call latency", attrs...)
	a.log.Info("call usage",
		"input_tokens", a.usage.Input,
		"output_tokens", a.usage.Output,
		"cache_read_tokens", a.usage.CacheRead,
		"cache_write_tokens", a.usage.CacheWrite,
	)
	a.log.Info("conversation ended", "reason", reason, "turns", a.replies)
}
