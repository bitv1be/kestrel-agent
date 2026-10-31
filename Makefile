GO ?= go
VERSION ?= dev
LDFLAGS = -s -w -X main.version=$(VERSION)
ARCHES = amd64 arm64 riscv64

.PHONY: build test bench lint dist clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o agent ./cmd/agent

test:
	CGO_ENABLED=1 $(GO) test -race ./...

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/...

lint:
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; fi

dist:
	@mkdir -p dist
	@for arch in $(ARCHES); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/agent-linux-$$arch ./cmd/agent || exit 1; \
	done
	@file dist/agent-linux-* 2>/dev/null || true

clean:
	@rm -f dist/agent-linux-*
