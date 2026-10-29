# Go 1.27.3, Debian trixie; multi-platform image indexes.
FROM docker.io/library/golang@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends librsync-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=1 go build -mod=readonly -trimpath -o /out/lode ./cmd/lode

# Debian trixie-slim.
FROM docker.io/library/debian@sha256:89c1ea91e897f0fe0a3731595330bc45b8bdef4047464a07116f891a94054b31

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates librsync2t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/lode /usr/local/bin/lode

ENTRYPOINT ["/usr/local/bin/lode"]
CMD ["serve", "/data"]
