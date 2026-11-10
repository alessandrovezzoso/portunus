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

.PHONY: build build-linux-amd64 build-linux-arm64 check lint integration echo run clean

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

# Tests against real providers. They carry the "integration" build tag and need
# API keys in the environment. There are none yet in milestone 0.
integration:
	go test -race -tags integration ./...

# Milestone 0: answer and echo the caller's audio back. The flag beats .env.
echo: build
	./bin/$(BINARY) -mode echo

# Run in the mode set in .env.
run: build
	./bin/$(BINARY)

clean:
	rm -rf bin
