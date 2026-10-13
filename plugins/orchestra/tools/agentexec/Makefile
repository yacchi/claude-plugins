# The go on PATH when it works, else the pinned toolchain through mise.
GO ?= $(shell go version >/dev/null 2>&1 && echo go || echo "mise exec go@$$(cat .go-version) -- go")

.PHONY: build test vet verify-goldens

build:
	$(GO) build -o bin/agent-exec ./cmd/agent-exec

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	GOOS=linux $(GO) build ./...

# Until the cutover the conformance goldens are Python's behavior: re-running
# the Python implementation against them must reproduce them exactly.
verify-goldens:
	$(GO) test -count=1 ./internal/conformance -impl=python
