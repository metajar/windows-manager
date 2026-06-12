# rewardd build automation.
#
# Produces static, CGO-free binaries with version metadata stamped in. The
# server is built for Linux (the "tight service"); the agent for Windows.
#
#   make build        host-native server + agent (dev)
#   make server       linux/amd64 server binary  -> dist/
#   make agent        windows/amd64 agent.exe    -> dist/
#   make release      both, for the default target matrix, + checksums
#   make test         unit tests with the race detector
#   make msi          Windows agent .msi (requires the WiX dotnet tool)
#   make clean

SHELL := /bin/bash

BIN_SERVER := rewardd-server
BIN_AGENT  := rewardd-agent

DIST := dist
PKG  := rewardd

# Version metadata. VERSION can be overridden: `make release VERSION=1.2.3`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

GOFLAGS := -trimpath
GO_ENV  := CGO_ENABLED=0

# Target matrix for `make release`.
SERVER_TARGETS := linux/amd64 linux/arm64 linux/arm
AGENT_TARGETS  := windows/amd64 windows/386 windows/arm64

.PHONY: all build server agent release test test-cover vet fmt tidy clean msi help

all: build

## build: host-native dev binaries into ./bin
build:
	$(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BIN_SERVER) ./cmd/server
	$(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BIN_AGENT) ./cmd/agent

## server: linux/amd64 server binary into ./dist
server:
	GOOS=linux GOARCH=amd64 $(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
		-o $(DIST)/$(BIN_SERVER)-linux-amd64 ./cmd/server

## agent: windows/amd64 agent into ./dist
agent:
	GOOS=windows GOARCH=amd64 $(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
		-o $(DIST)/$(BIN_AGENT)-windows-amd64.exe ./cmd/agent

## release: cross-compile the full target matrix + SHA256SUMS
release: clean
	@mkdir -p $(DIST)
	@for t in $(SERVER_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(DIST)/$(BIN_SERVER)-$$os-$$arch; \
		echo "  build $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/server || exit 1; \
	done
	@for t in $(AGENT_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(DIST)/$(BIN_AGENT)-$$os-$$arch.exe; \
		echo "  build $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/agent || exit 1; \
	done
	@cd $(DIST) && (sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS)
	@echo "release $(VERSION) -> $(DIST)/"

## test: race-enabled unit tests
test:
	go test -race ./...

## test-cover: tests with a coverage summary
test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## vet: go vet across all packages
vet:
	go vet ./...

## fmt: gofmt the tree
fmt:
	gofmt -s -w .

## tidy: tidy modules
tidy:
	go mod tidy

## msi: build the Windows agent installer (delegates to build/msi)
msi: agent
	VERSION=$(VERSION) AGENT_EXE=../../$(DIST)/$(BIN_AGENT)-windows-amd64.exe \
		bash build/msi/build-msi.sh

## clean: remove build artifacts
clean:
	rm -rf bin $(DIST) coverage.out

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
