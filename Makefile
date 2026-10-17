GO ?= go
GOFMT ?= gofmt
VERSION ?= dev

.PHONY: build check fmt fmt-check vet test

build:
	mkdir -p bin
	$(GO) build -buildvcs=false -ldflags "-X main.version=$(VERSION)" -o bin/lode ./cmd/lode

check: fmt-check vet test

fmt:
	$(GOFMT) -w cmd internal

fmt-check:
	@unformatted="$$($(GOFMT) -l cmd internal)" && \
	if [ -n "$$unformatted" ]; then \
		printf 'Run make fmt to format these files:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...
