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
make integration        # tests against real providers; needs API keys
make echo               # run in echo mode (milestone 0)
make run                # run with the mode set in .env
```

Run a single test: `go test ./internal/media/ -run TestPacerPadsUnderrun -v`

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
`internal/call`. Local engines (Whisper, Piper) run as separate HTTP services, so this
binary stays pure Go with no cgo.

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
1. **inbound**: RTP read → G.711 decode → resample → VAD and STT.
2. **outbound pacer**: a 20 ms ticker that writes exactly one frame per tick.
3. **orchestrator** (`internal/call`): the turn state machine (listening → thinking →
   speaking), barge-in, silence timeout and maximum call time.

One turn: VAD end of speech → STT final transcript → Claude streaming reply → sentence
splitter → speech sanitizer → TTS stream → resample → G.711 encode → pacer buffer.

The Claude model is set in config (`PORTUNUS_LLM_MODEL`) and is never hardcoded anywhere
else. Default to a fast Haiku-class model: on the phone, latency beats eloquence.
Conversation state lives in memory, one per call, and is discarded after notify. Nothing is
remembered across calls.

### Load-bearing details

- **Sample formats live only in `internal/media`.** The SIP leg is G.711 at 8 kHz. Adapters
  consume and produce PCM16 at the rate they declare. `internal/media` does every decode,
  encode and resample, and no other package touches sample formats or rates. If you are
  about to resample anywhere else, stop.
- **The output pacer is where call quality lives.** TTS emits audio in bursts, often faster
  than realtime, while RTP needs one 160-byte frame every 20 ms. The buffer absorbs the
  burst, the ticker drains it, and encoded silence pads underruns (`0xD5` A-law, `0xFF`
  µ-law).
  - The buffer is capped at `PORTUNUS_OUTPUT_BUFFER` (default 10 s) and drops the **oldest**
    bytes on overflow.
  - The cap must hold a full reply, or speech is cut mid-utterance. Barge-in flushes the
    buffer anyway, so a large cap costs nothing in interruptibility.
  - Writing audio "whenever it arrives" instead of on the tick produces choppy audio that
    sounds like a network problem.
- **Speak at the first sentence.** Never wait for the full Claude reply or the full TTS
  output. The sentence splitter hands each finished sentence to TTS while Claude is still
  writing the next one.
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

## Diagnostics: logs, not transcripts

The transcript records what was said, not what the caller heard, so it can look perfect
even when the audio was terrible. Every call ends with two log lines:

- `call audio quality`:
  - `starved_pct`: padding during an utterance, meaning the pipeline could not keep up with
    the RTP clock.
  - `dropped_bytes`: audio lost to buffer overflow.
  - `barge_ins` and `self_barge_ins`.
  - inbound RTP `lost_pct`: loss on the wire, downstream of this process.
- `call latency`, per turn:
  - `stt_ms`: end of speech → final transcript.
  - `llm_first_token_ms`.
  - `tts_first_audio_ms`.
  - `first_audio_ms`: end of speech → first frame on the wire.

  With three stages, "it's slow" cannot be diagnosed without per-stage numbers.

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
- **The registrar must resolve to a private address.** Use the S20's IP.
  `PORTUNUS_ALLOW_PUBLIC_REGISTRAR=true` is the explicit override.
- **The User-Agent is fixed to `Portunus/<version>`**, so the S20's User Agent Registration
  Authorization can be turned on. This is only defense in depth, because a User-Agent can be
  spoofed. The S20's IP restriction is the real control.
- **Callers are third parties.**
  - Recordings and transcripts are opt-in: an empty directory setting disables them.
  - Transcript text and audio never go into logs.
  - Caller numbers are masked at info level (`***123`) and appear in full only at debug.
- **Cost ceilings are features, not niceties.**
  - `PORTUNUS_MAX_CALL_TIME` limits the length of a call.
  - `PORTUNUS_MAX_CONCURRENT_CALLS` limits parallel calls. Beyond it, answer
    `486 Busy Here` and let the S20 forward the call.
  - The S20's own Max Call Duration on the extension is a second ceiling.
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

## Milestone ladder

Each early milestone stays available as a diagnostic mode (`PORTUNUS_MODE`, or `-mode`):

| Mode     | Proves                                                        | Needs         |
|----------|---------------------------------------------------------------|---------------|
| `echo`   | REGISTER, SDP, codec negotiation, RTP in both directions      | SIP only      |
| `greet`  | TTS → resample → encode → pacer: the caller hears a greeting  | TTS           |
| `listen` | VAD + STT: transcripts in the log, the assistant stays silent | STT           |
| `agent`  | the full conversation                                         | STT, LLM, TTS |

After `agent` come barge-in and timeouts, then notify, then deployment. Always prove `echo`
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
- **Extension settings:**
  - IP Restriction limited to the Portunus host.
  - No outbound routes.
  - DTMF Mode RFC4733 (RFC2833).
  - Qualify can stay on.
- **Fallback:** forward No Answer and When Busy to voicemail. Verify it by stopping Portunus
  and calling the extension.
- **Ring delay.** `PORTUNUS_RING_DELAY` defaults to `0`, because the S20 already decides
  when a call reaches Portunus.

## Open decisions: ask, do not choose

- **The first cloud STT and TTS providers.** Criteria: streaming, quality on Italian 8 kHz
  audio, latency, EU data processing.
- **The VAD implementation:** pure-Go energy-based, a model behind a local service, or the
  STT provider's own endpointing.
- **The HTTP API shape** of the local STT/TTS services.

## Not in v1

Call transfer (SIP REFER driven by Claude tool use) is planned for v2. Keep `internal/call`
able to end a turn with an action instead of speech, but do not implement it now. Outbound
calls are not a feature.
