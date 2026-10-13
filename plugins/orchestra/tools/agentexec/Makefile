# The go on PATH when it works, else the pinned toolchain through mise.
GO ?= $(shell go version >/dev/null 2>&1 && echo go || echo "mise exec go@$$(cat .go-version) -- go")

.PHONY: build test vet

build:
	$(GO) build -o bin/agent-exec ./cmd/agent-exec

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	GOOS=linux $(GO) build ./...
