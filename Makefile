# Portunus build and development tasks. CLAUDE.md explains what each mode proves.

BINARY  := portunus
PKG     := ./cmd/portunus

# The version ends up in the User-Agent header, so the S20 shows which build
# registered. "-dirty" marks a build from uncommitted changes.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# CGO_ENABLED=0 keeps the binary static and pure Go, so it runs on the Linux VM
# with nothing installed. -trimpath keeps local paths out of the binary.
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

COMPOSE := docker compose -f deploy/compose.yaml

.PHONY: build build-linux-amd64 build-linux-arm64 check lint integration echo greet run tts-up tts-down clean

build:
	$(GOBUILD) -o bin/$(BINARY) $(PKG)

# The production host.
build-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o bin/$(BINARY)-linux-amd64 $(PKG)

build-linux-arm64:
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o bin/$(BINARY)-linux-arm64 $(PKG)

check:
	go vet ./...
	go test -race ./...
	govulncheck ./...

lint:
	golangci-lint run

# Tests against real services. They carry the "integration" build tag. The TTS
# test needs the local Kokoro running: make tts-up first.
integration:
	go test -race -tags integration ./...

# Milestone 0: answer and echo the caller's audio back. The flag beats .env.
echo: build
	./bin/$(BINARY) -mode echo

# Milestone 1: answer, speak the greeting, hang up. Needs make tts-up.
greet: build
	./bin/$(BINARY) -mode greet

# The local speech services (deploy/compose.yaml), listening on 127.0.0.1.
tts-up:
	$(COMPOSE) up -d kokoro

tts-down:
	$(COMPOSE) down

# Run in the mode set in .env.
run: build
	./bin/$(BINARY)

clean:
	rm -rf bin
