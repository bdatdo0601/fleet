SHELL := /bin/sh
DEADCODE := $(or $(shell command -v deadcode 2>/dev/null),$(shell go env GOPATH)/bin/deadcode)
COMMIT := $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/bdatdo0601/fleet/internal/version.Commit=$(COMMIT) -X github.com/bdatdo0601/fleet/internal/version.BuiltAt=$(BUILT_AT)

.PHONY: fmt lint test check build

fmt:
	golangci-lint fmt ./...

lint:
	go vet ./...
	@diff=$$(go fix -diff ./...); if [ -n "$$diff" ]; then echo "$$diff"; exit 1; fi
	golangci-lint run ./...
	"$(DEADCODE)" ./cmd/fleet
	@if command -v node >/dev/null 2>&1; then \
		for f in internal/server/web/*.js; do \
			[ -f "$$f" ] || continue; \
			node --check "$$f" || exit 1; \
		done; \
	fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck shell/gauth.zsh bin/ssh-harden; \
	fi

test:
	go test -race -count=1 -timeout 120s ./...
	@if command -v node >/dev/null 2>&1 && [ -f internal/server/web_test.mjs ]; then \
		node --test internal/server/web_test.mjs; \
	fi

check: lint test

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o fleet ./cmd/fleet
