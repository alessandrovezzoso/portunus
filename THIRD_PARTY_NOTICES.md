# Third-party notices

Portunus is released under the MIT licence (see `LICENSE`). It links the Go
modules below. Each is used unmodified, and its full licence text ships with the
module source (`go mod download`, then the module's `LICENSE` file). A binary
release must bundle those texts alongside the binary.

The list is the set of modules linked into `bin/portunus`, as reported by
`go version -m bin/portunus`. Update it whenever `go.mod` changes.

| Module | Version | Licence |
|---|---|---|
| github.com/emiago/diago | v0.42.1 | MPL-2.0 |
| github.com/emiago/sipgo | v1.7.2 | BSD-2-Clause |
| github.com/emiago/dtls/v3 | v3.0.0-20260122183559-8b8d23e359c0 | MIT |
| github.com/go-audio/riff | v1.0.0 | Apache-2.0 |
| github.com/gobwas/httphead | v0.1.0 | MIT |
| github.com/gobwas/pool | v0.2.1 | MIT |
| github.com/gobwas/ws | v1.4.0 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/icholy/digest | v1.2.0 | MIT |
| github.com/pion/dtls/v3 | v3.1.5 | MIT |
| github.com/pion/ice/v4 | v4.3.0 | MIT |
| github.com/pion/logging | v0.2.4 | MIT |
| github.com/pion/mdns/v2 | v2.1.0 | MIT |
| github.com/pion/randutil | v0.1.0 | MIT |
| github.com/pion/rtcp | v1.2.16 | MIT |
| github.com/pion/rtp | v1.10.1 | MIT |
| github.com/pion/sdp/v3 | v3.0.18 | MIT |
| github.com/pion/srtp/v3 | v3.0.10 | MIT |
| github.com/pion/stun/v3 | v3.1.6 | MIT |
| github.com/pion/transport/v4 | v4.0.2 | MIT |
| github.com/pion/turn/v5 | v5.0.12 | MIT |
| github.com/wlynxg/anet | v0.0.5 | BSD-3-Clause |
| github.com/zaf/g711 | v1.4.0 | BSD-3-Clause |
| golang.org/x/crypto | v0.59.0 | BSD-3-Clause |
| golang.org/x/net | v0.62.0 | BSD-3-Clause |
| golang.org/x/sync | v0.16.0 | BSD-3-Clause |
| golang.org/x/sys | v0.50.0 | BSD-3-Clause |
| golang.org/x/time | v0.14.0 | BSD-3-Clause |

## Services that run alongside the binary

These are not linked into `bin/portunus`. They run as separate processes, in
containers started by `deploy/compose.yaml`, and Portunus talks to them over
HTTP on 127.0.0.1. They are listed so the licences of everything a deployment
runs sit in one place.

| Component | Version | Licence | Role |
|---|---|---|---|
| Kokoro-FastAPI (github.com/remsky/Kokoro-FastAPI) | v0.10.0 (image `ghcr.io/remsky/kokoro-fastapi-cpu`) | Apache-2.0 | HTTP server for the TTS |
| Kokoro-82M model weights (huggingface.co/hexgrad/Kokoro-82M) | as bundled by the image | Apache-2.0 | the TTS model and its Italian voice `if_sara` |

## Notes

- **diago (MPL-2.0).** The MPL applies per file, so no diago source may be
  copied into this repository's files. Portunus only imports it as a module.
- **Opus.** diago's cgo Opus binding (`gopkg.in/hraban/opus.v2`, MIT) appears in
  `go.sum`, but it sits behind the `with_opus_c` build tag. It is not compiled
  or linked, which keeps the binary pure Go.
- `go.sum` also lists modules used only by the tests of our dependencies. They
  are not linked into the binary and are not listed here.
