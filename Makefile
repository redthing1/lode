GO ?= go

.PHONY: fmt check build
fmt:
	gofmt -w cmd internal

check:
	@test -z "$$(gofmt -l cmd internal)" || (echo 'Run make fmt'; exit 1)
	$(GO) vet ./...
	$(GO) test ./...

build:
	mkdir -p bin
	$(GO) build -o bin/lode ./cmd/lode
