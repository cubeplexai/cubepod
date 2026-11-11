.PHONY: help build build-podd-init build-podd build-controller build-gateway test fmt vet lint clean

GO ?= go
BINDIR ?= $(CURDIR)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
LDFLAGS := -s -w -X github.com/cubeplexai/cubepod/pkg/version.Version=$(VERSION)

help:
	@echo "CubePod Build & Development Makefile"
	@echo ""
	@echo "Usage:"
	@echo "  make build              Build all binaries (podd-init, podd, cubepod-controller, cubepod-gateway)"
	@echo "  make build-podd-init    Build in-guest PID 1 OverlayFS mounter (podd-init)"
	@echo "  make build-podd         Build in-sandbox execution & file daemon (podd)"
	@echo "  make build-controller   Build Kubernetes Operator (cubepod-controller)"
	@echo "  make build-gateway      Build Unified API & Panel Proxy Gateway (cubepod-gateway)"
	@echo "  make test               Run all unit tests"
	@echo "  make fmt                Format Go code"
	@echo "  make vet                Run go vet across all packages"
	@echo "  make clean              Remove build artifacts"

build: build-podd-init build-podd build-controller build-gateway

build-podd-init:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/podd-init ./cmd/podd-init

build-podd:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/podd ./cmd/podd

build-controller:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/cubepod-controller ./cmd/cubepod-controller

build-gateway:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/cubepod-gateway ./cmd/cubepod-gateway

test:
	$(GO) test -v -race ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BINDIR)
