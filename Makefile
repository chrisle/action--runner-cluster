BINARY  := arc
PKG     := github.com/chrisle/action-runner-cluster
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

# Runner image coordinates. Override to publish under your own registry:
#   make images REGISTRY=ghcr.io/yourorg
REGISTRY ?= ghcr.io/chrisle
RUNNER_VERSION ?= 2.336.0

.PHONY: all
all: test build

.PHONY: build
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/arc

.PHONY: install
install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/arc

# Cross-compiled release binaries. The orchestrator is a single static binary
# with no runtime dependencies, so these drop straight onto a host.
.PHONY: release
release:
	@mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		echo "  building dist/$(BINARY)-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BINARY)-$$os-$$arch$$ext ./cmd/arc || exit 1; \
	done

.PHONY: test
test:
	go test ./...

# Unit tests only. Useful in CI where no Docker daemon is available.
.PHONY: test-unit
test-unit:
	ARC_SKIP_DOCKER_TESTS=1 go test ./...

.PHONY: test-race
test-race:
	go test -race ./...

.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@echo "html report: go tool cover -html=coverage.out"

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint: vet
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run ./... \
		|| echo "golangci-lint not installed; ran go vet only"

.PHONY: fmt
fmt:
	gofmt -w -s $$(find . -name '*.go' -not -path './vendor/*')

.PHONY: check
check: fmt vet test

# Verify every supported platform still compiles. The Windows and Unix process
# providers are behind build tags, so this is the only thing that catches a
# break in the one you are not developing on.
.PHONY: crosscheck
crosscheck:
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		GOOS=$$os GOARCH=$$arch go build -o /dev/null ./... \
			&& echo "  ok   $$os/$$arch" || { echo "  FAIL $$os/$$arch"; exit 1; }; \
	done

# --- runner images ----------------------------------------------------------

.PHONY: image-linux
image-linux:
	docker build \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		-t $(REGISTRY)/arc-runner:linux \
		images/linux

# Multi-arch build. Requires `docker buildx create --use` once.
.PHONY: image-linux-multiarch
image-linux-multiarch:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		-t $(REGISTRY)/arc-runner:linux \
		--push \
		images/linux

# Only works on a Windows host with Docker Desktop in Windows-container mode.
.PHONY: image-windows
image-windows:
	docker build \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		-t $(REGISTRY)/arc-runner:windows \
		images/windows

.PHONY: macos-template
macos-template:
	./scripts/setup-macos-template.sh

.PHONY: clean
clean:
	rm -rf bin dist coverage.out
