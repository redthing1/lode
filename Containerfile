FROM docker.io/library/golang:1.27.3-trixie AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends librsync-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=1 go build -trimpath -o /out/lode ./cmd/lode

FROM docker.io/library/debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates librsync2t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/lode /usr/local/bin/lode

ENTRYPOINT ["/usr/local/bin/lode"]
CMD ["serve", "/data"]
