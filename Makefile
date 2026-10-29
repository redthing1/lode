GO ?= go

.PHONY: fmt verify-deps check security build install
fmt:
	gofmt -w cmd internal

verify-deps:
	$(GO) mod tidy -diff
	$(GO) mod verify

check: verify-deps
	@test -z "$$(gofmt -l cmd internal)" || (echo 'Run make fmt'; exit 1)
	$(GO) vet -mod=readonly ./...
	$(GO) test -mod=readonly ./...

security: verify-deps
	$(GO) tool govulncheck ./...

build:
	mkdir -p bin
	$(GO) build -mod=readonly -o bin/lode ./cmd/lode

install:
	$(GO) install -mod=readonly ./cmd/lode
