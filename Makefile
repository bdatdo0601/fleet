SHELL := /bin/zsh
DEADCODE := $(or $(shell command -v deadcode 2>/dev/null),$(shell go env GOPATH)/bin/deadcode)

.PHONY: fmt lint test check build

fmt:
	golangci-lint fmt ./...

lint:
	go vet ./...
	@diff=$$(go fix -diff ./...); if [ -n "$$diff" ]; then echo "$$diff"; exit 1; fi
	golangci-lint run ./...
	"$(DEADCODE)" ./cmd/fleet

test:
	go test -race -count=1 -timeout 120s ./...

check: lint test

build:
	CGO_ENABLED=0 go build -o fleet ./cmd/fleet
