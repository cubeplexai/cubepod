.PHONY: help build build-rust build-podd-init build-podd build-go build-controller build-gateway build-egress test test-rust test-go test-e2e test-k8s-e2e fmt vet clean

GO ?= go
CARGO ?= $(shell which cargo 2>/dev/null || echo "$(HOME)/.cargo/bin/cargo")
BINDIR ?= $(CURDIR)/bin
CUBEPOD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.2.0")
LDFLAGS := -s -w -X github.com/cubeplexai/cubepod/pkg/version.Version=$(CUBEPOD_VERSION)

help:
	@echo "CubePod Build & Development Makefile (Rust In-Guest Crates + Go Control Plane)"
	@echo ""
	@echo "Usage:"
	@echo "  make build              Build all binaries (Rust: podd-init, podd | Go: cubepod-controller, cubepod-gateway, cubepod-egress)"
	@echo "  make build-podd-init    Build Rust PID 1 OverlayFS pivot mounter (crates/podd-init)"
	@echo "  make build-podd         Build Rust in-guest execution daemon (crates/podd)"
	@echo "  make build-controller   Build Go Kubernetes Operator (cmd/cubepod-controller)"
	@echo "  make build-gateway      Build Go Unified API & Panel Proxy Gateway (cmd/cubepod-gateway)"
	@echo "  make build-egress       Build Go Zero-Trust Egress Sidecar Proxy (cmd/cubepod-egress)"
	@echo "  make test               Run all Rust and Go unit tests"
	@echo "  make test-e2e           Run full multi-process end-to-end integration tests"
	@echo "  make fmt                Format Rust and Go source code"
	@echo "  make clean              Remove build artifacts"

build: build-podd-init build-podd build-controller build-gateway build-egress

build-podd-init:
	@mkdir -p $(BINDIR)
	$(CARGO) build --release -p podd-init
	cp target/release/podd-init $(BINDIR)/podd-init

build-podd:
	@mkdir -p $(BINDIR)
	$(CARGO) build --release -p podd
	cp target/release/podd $(BINDIR)/podd

build-controller:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/cubepod-controller ./cmd/cubepod-controller

build-gateway:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/cubepod-gateway ./cmd/cubepod-gateway

build-egress:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/cubepod-egress ./cmd/cubepod-egress

test: test-rust test-go

test-rust:
	$(CARGO) test --workspace

test-go:
	$(GO) test -v -race ./...

test-e2e: build
	$(GO) test -v -race ./test/e2e/...

test-k8s-e2e:
	CUBEPOD_K8S_E2E=1 $(GO) test -v -timeout 5m ./test/k8s-e2e/...

fmt:
	$(CARGO) fmt --all
	$(GO) fmt ./...

vet:
	$(CARGO) clippy --workspace --all-targets -- -D warnings
	$(GO) vet ./...

clean:
	rm -rf $(BINDIR) target
