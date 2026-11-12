# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Working on this repo

- Talk to me in Italian. Code, comments, commit messages and docs are in English.
- One milestone at a time (see the ladder below). Propose the step, implement it, then stop
  so I can verify it on a real phone before moving on. Do not build ahead of the current
  milestone.
- This is a learning and portfolio project: prefer clear code over clever code, and write
  comments that explain *why*, not *what*.
- Ask before adding a dependency, choosing a provider, or changing an interface in `internal/`.

## Status

Pre-alpha, built milestone by milestone. Parts of this file describe the target design, not
code that exists yet. If the code and this file disagree, point it out instead of silently
"fixing" either one.

## Commands

```sh
make build              # -> bin/portunus (native; fine on macOS for echo tests)
make build-linux-amd64  # the production host
make build-linux-arm64
make check              # go vet + go test -race ./... + govulncheck
make lint               # golangci-lint
make integration        # tests against real services; needs make tts-up and make stt-up
make echo               # run in echo mode (milestone 0)
make greet              # run in greet mode (milestone 1); needs make tts-up
make listen             # run in listen mode (milestone 2); needs make stt-up
make agent              # run in agent mode (milestone 3); needs both services and the API key
make run                # run with the mode set in .env
make tts-up             # start Kokoro (deploy/compose.yaml)
make tts-down           # stop it
make stt-up             # build and start Whisper; the first build takes minutes
make stt-down           # stop it
```

Run a single test: `go test ./internal/media/ -run TestPacerSendsFullFramesAndPadsWithSilence -v`

Config is environment-driven, and the binary reads `.env` from its working directory
itself, so do not source it first. Precedence is **flag > `.env` > shell environment**. The
file beats the shell so that a stale `set -a && . ./.env` export cannot silently override an
edited file. The `-mode` flag exists so that a diagnostic mode cannot be overridden by
whatever `.env` says. `PORTUNUS_ENV_FILE` points to a different file. Variables are prefixed
`PORTUNUS_`, except `ANTHROPIC_API_KEY`, which keeps the name the official SDK expects.

## What this is

An AI answering service for a SIP PBX. Portunus registers with a Yeastar S20 as an ordinary
SIP extension and answers the calls the PBX routes to it. It talks with the caller in
Italian, takes a message, and delivers a summary. It is one static binary and nothing is
exposed to the internet. v1 is the answering service only.

Four decisions shape everything else:

**The PBX is the PBX.** Portunus is one extension. The S20 decides when calls reach it
(out of hours, no answer, an IVR key), not this code. Do not add routing rules, a dial plan,
or a second SIP server.

**Cascaded pipeline, not speech-to-speech.** Claude takes and returns text, so each turn is
STT → Claude → TTS. Latency is the central problem of this project, so everything streams
(see below).

**Providers are pluggable.** `internal/call` depends only on the `STT`, `LLM`, `TTS` and
`VAD` interfaces. `cmd/portunus` picks the adapters from config, and adapters never import
`internal/call`. Local engines (Kokoro for TTS, whisper.cpp for STT) run as separate HTTP
services, so this binary stays pure Go with no cgo. The VAD is the exception: Silero is
small enough to run in process, through govad, a pure Go implementation.

**Callers speak Italian, on narrowband phone audio.** Every STT/TTS adapter and every prompt
is judged on Italian 8 kHz telephone audio, not on English demos.

## Architecture

Call flow, end to end:

1. `cmd/portunus` loads config, resolves the LAN bind address and runs the preflight checks.
   SIP puts the address in the message body, so binding to the wildcard yields an
   unreachable Contact.
2. `internal/sip` registers and serves.
3. On INVITE: concurrency check (`486 Busy Here` when full) → `Trying` → `Ringing` → ring
   delay → answer with codecs pinned.
4. `internal/call` runs the session, and `internal/notify` gets the result.

Each call runs three goroutines, all cancelled by a shared context:
1. **inbound**: RTP read → G.711 decode → resample → VAD → segmenter. Finished utterances go
   to a transcriber goroutine through a short queue, so a slow STT delays transcripts but
   never the RTP read. An utterance captured before a hangup is still transcribed, within a
   drain deadline, because the last phrase is often the one that matters.
2. **outbound pacer**: writes exactly one frame every 20 ms. The 20 ms clock is diago's RTP
   writer, whose every `Write` blocks until its next tick. The pacer feeds it one frame per
   `Write`, and silence when its queue is empty.
3. **orchestrator** (`internal/call`): the turn state machine (listening → thinking →
   speaking), barge-in, silence timeout, maximum call time and maximum turns. In agent
   mode the ear (`ear.go`) reads inbound audio in its own goroutine, and the mouth
   (`mouth.go`) synthesizes a reply's sentences in another while the model streams the
   next ones.

One turn: VAD end of speech → STT final transcript → Claude streaming reply → sentence
splitter → speech sanitizer → TTS stream → resample → G.711 encode → pacer buffer.

The Claude model is set in config (`PORTUNUS_LLM_MODEL`) and is never hardcoded anywhere
else. Default to a fast Haiku-class model: on the phone, latency beats eloquence. The
system prompt is fixed, so it is sent with prompt caching; nothing that changes per call
(the time, the caller number) may sit before the cache breakpoint.
Conversation state lives in memory, one per call, and is discarded after notify. Nothing is
remembered across calls.

### Load-bearing details

- **Sample formats live only in `internal/media`.** The SIP leg is G.711 at 8 kHz. Adapters
  consume and produce PCM16 at the rate they declare. `internal/media` does every decode,
  encode and resample, and no other package touches sample formats or rates. That includes
  the float conversion a model needs (`media.ToFloat32`) and the WAV container an STT
  upload needs (`media.EncodeWAV`). If you are about to resample anywhere else, stop.
- **The output pacer is where call quality lives.** TTS emits audio in bursts, often faster
  than realtime, while RTP needs one 160-byte frame every 20 ms. The buffer absorbs the
  burst, the 20 ms clock drains it, and encoded silence pads underruns (`0xD5` A-law,
  `0xFF` µ-law).
  - The buffer is capped at `PORTUNUS_OUTPUT_BUFFER` (default 10 s) and drops the **oldest**
    bytes on overflow.
  - The cap must hold a full reply, or speech is cut mid-utterance. Barge-in flushes the
    buffer anyway, so a large cap costs nothing in interruptibility.
  - Writing audio "whenever it arrives" instead of on the tick produces choppy audio that
    sounds like a network problem.
- **Speak at the first sentence.** Never wait for the full Claude reply or the full TTS
  output. The sentence splitter hands each finished sentence to TTS while Claude is still
  writing the next one.
- **Until barge-in exists, the two sides take turns.** While the assistant is audible, and
  for a 300 ms echo tail after, the ear is muted: speech heard then is counted
  (`ignored_utterances`) and thrown away. That also keeps the handset's echo of the
  assistant out of the conversation. Speech heard while the assistant is only thinking is
  kept and answered next.
- **The holding phrase covers the wait.** The moment the caller stops talking, a short
  phrase synthesized at startup plays at once ("Certo, un momento."), in rotation from
  `prompts/holding.txt`, while the STT and the model work. The prompt tells the model it
  exists, so it does not open its reply with another one.
- **Barge-in.** When VAD hears the caller start speaking while the assistant is audible:
  1. Flush the pacer buffer. This is what silences the assistant.
  2. Cancel the turn's context. This stops the Claude stream and TTS.

  "Audible" means `speaking || pacer.Len() > 0`. The buffer holds audio the caller has not
  heard yet, so generation often finishes while the assistant is still talking.
- **Self barge-ins are counted separately.** If the handset echoes the assistant back, VAD
  hears it as the caller interrupting. Its fix is the opposite of a slow pipeline's (raise
  the VAD threshold rather than cut latency), so it gets its own counter.
- **Speech sanitizer before TTS.** The model's text goes to an ear, not a screen, so strip
  markdown, lists, emojis and URLs. The prompt asks for this too, but the sanitizer is the
  guarantee.
- **Codecs are pinned** to PCMA/PCMU plus `telephone-event` (RFC 4733 DTMF, kept for a
  future "press 1"). G.722 could help STT on internal Yealink calls, but it needs its own
  codec in `internal/media`, so it is not in v1.
- Registration runs in a retry loop with exponential backoff (1 s → 60 s), so an S20 reboot
  or firmware update does not take the service down.
- **Notification is email-only in v1, and configured in one place.**
  - Every setting lives in config: the `PORTUNUS_SMTP_*` server settings and
    `PORTUNUS_NOTIFY_TO` (comma-separated recipients). Nothing else in the code holds an
    address, a server or a subject line.
  - SMTP requires TLS (STARTTLS or implicit TLS). Credentials are never sent in clear.
  - `internal/notify` exposes a `Notifier` interface, so other channels can be added later
    without touching `internal/call`.
- **Notification is best-effort and must never fail a call.**
  - When storage is enabled, the transcript is written to disk before any network delivery
    is attempted.
  - Preflight checks the SMTP server and credentials. A failure is only a warning, so calls
    are still answered.
  - A failed delivery is logged at error level with the call ID: with storage disabled, that
    message is otherwise lost.

### diago quirks (as of the version in go.mod)

- The RTP port range is a package-level global (`media.RTPPortStart/End`), not a
  per-instance option. Set it once at startup.
- diago answers SIP `OPTIONS` with `200 OK` on its own, and the S20's Qualify check depends
  on it. If REGISTER succeeds but the S20 shows the extension as unreachable, check OPTIONS
  first.
- `diago.AudioStereoRecordingWav` contains a mutex and must never be copied. `go vet`
  catches this.
- **diago's RTP writer is the 20 ms clock.** Each `Write` sends one packet, then blocks on
  the writer's own ticker. Never call `ClockDisable`: it leaves the ticker nil, and the next
  `Write` panics. A second ticker in the pacer would only fight this one.
- **diago answers only SDP offers with a single `m=` line.** It counts every `m=` line,
  whatever its type, and rejects the offer with `more than 1 media line for type "audio"`.
  An S20 offer with `m=audio` plus `m=video` therefore cannot be answered, so the S20's video
  codecs must stay disabled (see the Yeastar notes). The correct fix, answering the extra
  lines with port 0 as RFC 3264 requires, needs a patch to diago. It is deferred: upstream
  `main` has the same limit, and no upstream issue reports it.
- **diago answers `NOTIFY` outside a dialog with `481`.** The S20 sends an unsolicited
  `Event: message-summary` NOTIFY (voicemail state) about every 30 s, so `internal/sip`
  answers those with `200 OK` in a request middleware before diago sees them.
- **diago hangs up an unanswered call with `480`,** which the S20 announces to the caller as
  "extension not available". `internal/sip` refuses calls with `486` instead, so the S20's
  When Busy forward sends the caller to voicemail.
- **Known log noise is demoted to debug** (`libNoise` in `internal/sip`): the S20's RTCP
  receiver reports carry SSRC 1 instead of ours, and duplicate responses can arrive after a
  transaction has ended at shutdown. Neither affects calls.

## Diagnostics: logs, not transcripts

The transcript records what was said, not what the caller heard, so it can look perfect
even when the audio was terrible. Every call ends with these log lines:

- `call audio quality`, in the modes that speak:
  - `starved_pct`: padding during an utterance, meaning the pipeline could not keep up with
    the RTP clock.
  - `dropped_bytes`: audio lost to buffer overflow.
  - `barge_ins` and `self_barge_ins`.
- `call latency`, per turn:
  - `stt_ms`: from the VAD deciding the utterance is over to the final transcript. The
    caller waits `PORTUNUS_VAD_SILENCE` more than this, because the VAD needs that much
    silence to decide.
  - `llm_first_token_ms`.
  - `tts_first_audio_ms`.
  - `first_audio_ms`: end of speech → first frame on the wire.

  With three stages, "it's slow" cannot be diagnosed without per-stage numbers. In listen
  mode each utterance gets its own `utterance` line (`speech_ms`, `stt_ms`, `chars`), and
  `call latency` sums them up (`turns`, `stt_ms_avg`, `stt_ms_max`).
- `call ended`, written by `internal/sip` in every mode: duration, frames in and out, and
  inbound RTP `lost_pct`, the loss on the wire before this process.

In agent mode each answered turn logs `turn latency` (`stt_ms`, `llm_first_token_ms`,
`tts_first_audio_ms`, `first_audio_ms`; the last runs from the VAD's end-of-speech decision
to the first frame of the real reply on the wire, after the holding phrase). The call ends
with `call audio quality` (plus `ignored_utterances` and `dropped_utterances`), `call
latency` (averages and maxima), `call usage` (input, output, cache read and cache write
tokens, so the cost of a call can be read from it) and `conversation ended` with its reason:
`assistant`, `caller`, `max_turns`, `max_call_time`, `llm_unavailable`, `llm_error` or
`tts_error`. Both sides of the conversation appear only at debug level (`turn text`).

When recording is enabled, the WAV is tapped inside the process, before the wire. A clean
WAV next to choppy audio at the handset moves the fault to RTP delivery or the phone.

## Security and privacy

- **Secrets are logged as fingerprints, never in full** (`config.Fingerprint`). A mismatch
  between the intended and the effective credential can then be diagnosed without the secret
  appearing in a log.
- **Credentials are checked at startup, not mid-call.** Before registering, preflight
  verifies the Anthropic key and every enabled cloud provider.
  - A rejected key is fatal. Portunus does not register, so the S20 sends calls to its
    fallback instead of a dead line.
  - An unreachable API is only a warning.
- **In agent mode, a call is answered only if it can be served.** On every INVITE,
  `sip.Options.Ready` pings Kokoro and Whisper (300 ms at most) and checks that the model
  account has not been marked down. Otherwise the call gets `486` and goes to voicemail.
  Exhausted credit or a revoked key (402, 401, 403) marks the account down; a one-token
  probe then runs at most once a minute, and the first success opens the door again. The
  diagnostic modes have no such check: seeing the failure is their point.
- **The registrar must resolve to a private address.** Use the S20's IP.
  `PORTUNUS_ALLOW_PUBLIC_REGISTRAR=true` is the explicit override.
- **The User-Agent is fixed to `Portunus/<version>`**, so the S20's User Agent Registration
  Authorization can be turned on. This is only defense in depth, because a User-Agent can be
  spoofed. The S20's IP restriction is the real control.
- **Callers are third parties.**
  - Recordings and transcripts are opt-in: an empty directory setting disables them.
  - Transcript text and audio never go into logs at info level. The listen mode logs
    transcripts at debug level only, like full caller numbers, so they can be checked
    against what was said during a test.
  - Caller numbers are masked at info level (`***123`) and appear in full only at debug.
- **Cost ceilings are features, not niceties.** The Messages API is stateless, so every turn
  resends the whole conversation and a long call costs more per turn. The real risk is not a
  normal call but one that never ends: an open line, an answering machine, or the assistant
  transcribing and answering its own echo. STT and TTS bill by time or characters, so the
  main ceilings bound time and turns, not only tokens. They are layered:
  - Per reply: a small `max_tokens`. Phone replies are short.
  - Per call: `PORTUNUS_MAX_CALL_TIME` limits the length of a call, and `PORTUNUS_MAX_TURNS`
    limits its turns, which stops an echo loop long before the time limit.
  - `PORTUNUS_MAX_CONCURRENT_CALLS` limits parallel calls. Beyond it, answer
    `486 Busy Here` and let the S20 forward the call.
  - The S20's own Max Call Duration on the extension is a second ceiling.
  - Per account: a spend limit in the Anthropic Console, on a workspace and API key used only
    by Portunus.
  - A ceiling ends the call gracefully: a short closing line, then BYE, never a cut
    mid-sentence. The message taken so far is still delivered.
  - If the API reports exhausted credit or quota, Portunus stops answering, so the S20 sends
    calls to its fallback instead of to a mute assistant.
  - The end-of-call log reports the tokens used (input, output, cache reads), so cost can be
    read per call.
- **The repository is public.** Never commit the office's real IP addresses, phone numbers,
  extension numbers, company names, or the real prompt. Examples use documentation addresses
  (`192.0.2.0/24`). These are gitignored: `.env`, `prompts/*` except `prompts/example.*`,
  `recordings/` and `transcripts/`.
- **Dependency licences must be compatible with MIT.** MIT, BSD, Apache-2.0 and MPL-2.0 are
  fine; GPL and AGPL are not. Every dependency is listed in `THIRD_PARTY_NOTICES.md`. Never
  copy diago source into our files, because MPL-2.0 applies per file.
- **phonellm is inspiration, not a source.** It has no licence, so its code must not be
  copied, adapted or translated here. Its design ideas are fine, but the implementation is
  ours.

## Threat model: what a caller can try

Callers are strangers, and some will try to misuse the assistant. v1 is built so that the
worst they can do is waste a call.

- **The system prompt must be treated as public.** A caller can ask the model to repeat
  its instructions, and no prompt reliably prevents that. Nothing a caller must not hear
  goes into `prompts/system.txt`: no internal numbers, names of clients, passwords or
  procedures. Office information the assistant may give is listed there explicitly.
- **The caller ID is a claim, not an identity.** The number in From can be spoofed, and
  the model never sees it anyway. Nothing is ever unlocked by who the caller says they are.
- **The model has one tool, `end_call`, and it only hangs up.** It cannot look anything
  up, send anything, or reach the LAN. Every future capability (call transfer in v2 first)
  takes its limits from config, never from the model: the model may ask to transfer, but
  the list of extensions it may transfer to is not its decision.
- **The message summary is caller-controlled text.** When notify arrives, the email must
  mark the summary as unverified and render links as plain text: "the director says to
  wire money to this account" is exactly what a fraudster would dictate.
- **Prompt injection through speech** ("ignore your instructions…") can at worst make the
  model say something off-script to the very person who asked for it. The prompt keeps it
  on its task; the sanitizer and the lack of tools keep anything else from happening.
- **Callers must know they are talking to a machine** (EU AI Act transparency): the
  greeting says so before anything else, and it is fixed text, not generated.
- **What callers say leaves the office for Claude** (Anthropic), as text. Audio stays
  local (Whisper and Kokoro run on this host). The office's privacy notice must say so,
  and so must any notice about transcripts or recordings once they are enabled.

## Milestone ladder

Each early milestone stays available as a diagnostic mode (`PORTUNUS_MODE`, or `-mode`):

| Mode     | Proves                                                        | Needs         |
|----------|---------------------------------------------------------------|---------------|
| `echo`   | REGISTER, SDP, codec negotiation, RTP in both directions      | SIP only      |
| `greet`  | TTS → resample → encode → pacer: the caller hears a greeting  | TTS           |
| `listen` | VAD + STT: transcripts at debug level, the assistant silent   | STT           |
| `agent`  | the full conversation                                         | STT, LLM, TTS |

The cost ceilings (`max_tokens`, `PORTUNUS_MAX_CALL_TIME`, `PORTUNUS_MAX_TURNS`) ship with
`agent` itself: no real conversation runs without them, not even a test call. After `agent`
come barge-in and the silence timeout, then notify, then deployment. Always prove `echo`
first. When debugging anything audio-related, drop to the lowest mode that reproduces the
fault before suspecting a provider.

## Testing

- `internal/media` is made of pure functions. Test it with golden samples in `testdata/`.
- `internal/call` is tested with fake STT, LLM, TTS and VAD that script whole conversations
  deterministically, including barge-in, timeouts and cancellation.
- No unit test touches the network or a real API. Tests against real providers carry
  `//go:build integration` and run through `make integration`.
- Always run with `-race`.

## Deployment

- **Host:** a Linux x86-64 VM on the office vSphere, with a static IP.
- **Service:** `deploy/portunus.service` (systemd, `EnvironmentFile`, `StateDirectory`).
- **Network:** the VM either sits in the S20's subnet, or the firewall allows `5060/udp` and
  the RTP range (`PORTUNUS_RTP_PORT_MIN`–`MAX`) between the VM and the S20.
- **CPU:** the 20 ms pacer needs the CPU on time. If `starved_pct` rises while the providers
  are fast, check the VM's CPU Ready time in vSphere before blaming the network.
- **Local services:** `deploy/compose.yaml` runs the optional local STT/TTS services.
- **Development:** on macOS (Apple Silicon).

## Yeastar S20 notes

- **Dedicated SIP extension.** The extension number (`PORTUNUS_SIP_EXTENSION`) and the
  Registration Name used for digest auth (`PORTUNUS_SIP_AUTH_USER`) are separate settings.
  The auth user defaults to the extension number when empty.
- **Codecs.** On the S-Series, codecs are set globally, not per extension. PCMA/PCMU must be
  enabled there, because Portunus's SDP answer accepts G.711 only.
  - The video codecs (H.264, H.261, H.263, H.263p) must be **disabled**: with any of them
    enabled, the S20 adds an `m=video` line that diago cannot answer, and every call fails.
    This applies to every extension, so it only fits an office that makes no video calls.
  - The S20 offers PCMU first, so calls negotiate PCMU. Either G.711 law is fine.
  - The README's S20 setup section must state the video requirement when it is written.
- **Extension settings:**
  - IP Restriction limited to the Portunus host.
  - No outbound routes.
  - DTMF Mode RFC4733 (RFC2833).
  - Qualify can stay on.
- **Fallback:** forward No Answer and When Busy to voicemail. Verify both paths:
  - stop Portunus and call the extension: voicemail answers with its "not available"
    greeting;
  - set `PORTUNUS_MAX_CONCURRENT_CALLS=1` and place two calls: the second gets `486`, and
    voicemail answers with its "on the phone" greeting.
- **Ring delay.** `PORTUNUS_RING_DELAY` defaults to `0`, because the S20 already decides
  when a call reaches Portunus.

## Kokoro notes

- **It runs as Kokoro-FastAPI in a container** (`deploy/compose.yaml`), with the image tag
  pinned and the port bound to `127.0.0.1:8880`. Portunus calls it through
  `internal/tts/openaicompat`.
- **Its text normalizer is turned off** (`normalization_options.normalize: false`), because
  it is written for English. Left to espeak-ng, `02 4567 8901` is read as large numbers
  ("quattromilacinquecento…"), not digit by digit. `internal/speech` does Portunus's own
  Italian normalization before every sentence: numbers in words, phone numbers digit by
  digit in their groups, times, dates, amounts and e-mail addresses. Kokoro's inline pause
  tags (`[pause:0.4s]`) are left untouched.
- **It is weak on very short utterances** (fewer than 10–20 tokens), so the sentence splitter
  merges sentences shorter than 25 characters with the next one, and the holding phrases
  are a little longer than two words.
- **The voice prefix picks the language.** `if_sara` selects the Italian pipeline, so no
  `lang_code` is sent.
- **Speed:** about 9× real time on an M3 Pro CPU, and slower on the first synthesis after
  start. Measure the VM's CPU before deploying.

## Whisper notes

- **It runs as whisper.cpp's server in a container built locally** (`deploy/whisper/
  Dockerfile`), not pulled. The whisper.cpp release and the model file are pinned there, and
  the model's sha256 is verified. The build uses every instruction of the CPU it runs on
  (`GGML_NATIVE`), so build the image on the host that runs it and never push it. The port
  is `127.0.0.1:8890`. `--inference-path` gives it the OpenAI-style endpoint that
  `internal/stt/openaicompat` speaks.
- **The model is large-v3-turbo, quantized to q5_0** (574 MB). Its transcription of Italian
  phone audio (300–3400 Hz, µ-law) synthesized by Kokoro was word for word right, including
  names and a phone number.
- **Its cost does not shrink with the utterance.** The encoder always processes a whole
  window, so a 3 s phrase costs as much as a 10 s one. Measured in Docker on an M3 Pro, with
  the full 30 s window: about 11.6 s per utterance with 4 threads, 8.3 s with 10. Too slow
  for a conversation.
- **So the window is reduced: `--audio-ctx 512`, about 10 s, with 8 threads.** That takes
  2.5–2.7 s per utterance on the same Mac, still word for word right on the test phrases
  (synthesized speech: real voices are checked at the phone test). Utterances must then fit
  in the window: `PORTUNUS_STT_MAX_UTTERANCE` (9.5 s) cuts longer speech into pieces.
  Change the two together, or the end of every long utterance is silently lost. The VM's
  numbers will differ.
- **It serves one request at a time.** Concurrent calls queue on it, and so do parallel
  tests: `make integration` runs packages one at a time.
- **It invents text for silence or noise** ("Sottotitoli a cura di…", "Grazie per la
  visione"). The VAD is the first defence, because only speech reaches Whisper.
  `--suppress-nst` drops non-speech tokens. A filter for known phrases comes only if calls
  show the need.
- **The language is fixed to Italian** (`--language it` on the server, `language=it` in each
  request). Detection would cost time and can guess wrong on a short phrase.
- **The preflight check transcribes one second of silence.** It proves the server and the
  model work, and it warms the model up, so the first call does not pay for a cold start.

## Silero VAD notes

- **It runs in process through govad** (github.com/zserge/govad): pure Go, no cgo, Silero v5
  weights embedded (about 1.2 MB of the binary), checked by its author against the ONNX
  model. It costs about 0.6 ms per 32 ms frame on an M3 Pro, about 2% of one core per call.
- **It accepts 16 kHz only**, so the 8 kHz call audio is upsampled first. One detector per
  call, because it keeps LSTM state.
- **The segmenter turns its probabilities into utterances** (`internal/call/segmenter.go`):
  - speech starts at `PORTUNUS_VAD_THRESHOLD` and ends below the threshold minus 0.15;
  - an utterance ends after `PORTUNUS_VAD_SILENCE` of silence;
  - 300 ms of audio before the detected start are kept;
  - bursts under 250 ms are dropped;
  - a monologue is cut at `PORTUNUS_STT_MAX_UTTERANCE`, so each piece fits Whisper's window.

## Claude notes

- **Model: `claude-haiku-5-5`**, from `PORTUNUS_LLM_MODEL`, through Anthropic's official Go
  SDK (`internal/llm/anthropic`). Effort is `low`, and thinking is off by default
  (`PORTUNUS_LLM_THINKING`): on the phone every second of silence counts. Haiku 5.5 accepts
  thinking off at effort `high` or below. With thinking off, the adapter appends Anthropic's
  recommended line against tool calls written as text.
- **No sampling parameters** (`temperature`, `top_p`): current models reject them.
- **Prompt caching:** two breakpoints. One on the fixed system prompt; the call's own context
  (date, time, the greeting already said) is a second system block after it. The other is
  the top-level `cache_control`, which caches the conversation so far on every turn. Haiku
  5.5 caches prefixes from 512 tokens, so a short prompt starts caching after a turn or two.
  `call usage` shows the cache reads.
- **The caller's number is never sent to Claude.** It is a third party's data, and the
  assistant asks for a callback number anyway.
- **The adapter keeps the history, thinking blocks included and unmodified**, as the API
  requires. A failed reply leaves the history as it was.
- **This SDK version sends the key as `Authorization: Bearer`**, not `x-api-key`. It also
  honors `ANTHROPIC_BASE_URL` and `ANTHROPIC_CUSTOM_HEADERS` from the environment: leave
  them unset on the VM.
- **Errors:** 401 and 403 are fatal at startup and mark the account down during calls; 402
  (`billing_error`) marks it down; a 400 mentioning "credit balance" is treated the same,
  as a best effort, because the SDK warns that error wording can change. `Models.Get`
  (the startup check) costs nothing and works with no credit, which is why the recovery
  probe is a real one-token request.
- **Refusals** (`stop_reason: refusal`) get a fixed neutral line, and the call goes on.
  Haiku 5.5 has no server-side fallback model.
- **The greeting is fixed text in `prompts/agent-greeting.txt`, not generated:** it plays at
  once and always carries the AI Act notice. It is about 14 s long; a shorter one gives
  the caller the floor sooner.

## Decisions taken

- **TTS: Kokoro, with the female Italian voice `if_sara`, run locally** (see the Kokoro
  notes). It was chosen in a blind listening test at telephone quality (300–3400 Hz, 8 kHz,
  G.711 µ-law round trip): free, Apache-2.0, and no text leaves the office. Pocket TTS lost
  that test: numbers and times sounded garbled, and the voice was not natural enough.
- **Local TTS API: the OpenAI-style `POST /v1/audio/speech`**, with `response_format: "pcm"`
  (24 kHz, 16-bit little-endian mono) and `stream: true`. Any compatible server works with
  `internal/tts/openaicompat`, so changing the engine is a configuration change.
- **STT: Whisper large-v3-turbo, run locally on whisper.cpp** (see the Whisper notes). It
  is free and MIT, and no caller's voice leaves the office. The cost is latency: Whisper
  transcribes an utterance only once it is over, and on a CPU that takes seconds.
- **Local STT API: the OpenAI-style `POST /v1/audio/transcriptions`**, a multipart upload of
  one WAV utterance (16 kHz, 16-bit mono) with `language` and `response_format: "json"`.
  Any compatible server works with `internal/stt/openaicompat`.
- **LLM: Claude Haiku 5.5 through Anthropic's official Go SDK** (Apache-2.0). See the Claude
  notes. The model holds the conversation and takes the message; it has one tool,
  `end_call`.
- **The agent mode takes turns until barge-in exists**, and fills the wait after each phrase
  with a pre-synthesized holding phrase (the user's choice, after the first latency
  numbers: about 3 s of listening and transcription before the model even starts).
- **VAD: Silero, in process through govad** (see the Silero VAD notes), rather than a
  loudness threshold, which phone line noise and echo fool, or a separate service, which
  would add a container and a network hop per frame.

## Open decisions: ask, do not choose

- **Thinking on or off.** `TestFirstTokenLatency` (integration, needs the API key) measures
  the delay before the first word both ways. Off is the default until the numbers say
  otherwise.

- **Whisper's speed against its accuracy.** The reduced window (`--audio-ctx 512`) was
  chosen for speed; whether it stays accurate on real callers' voices is checked at the
  phone test. The model size and the threads are settled with measurements on the VM
  before deployment.

## Not in v1

Call transfer (SIP REFER driven by Claude tool use) is planned for v2. Keep `internal/call`
able to end a turn with an action instead of speech, but do not implement it now. Outbound
calls are not a feature.
