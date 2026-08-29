# Build and runtime are separate stages so the shipped image carries the binary
# and nothing else: no toolchain, no module cache, no source.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not invalidate the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO off for a static binary that runs on a scratch-adjacent base.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/switchyard ./cmd/switchyard

FROM alpine:3.22

# wget is in busybox already and is what the compose healthcheck uses; adding
# ca-certificates keeps an OTLP endpoint over TLS working if one is configured.
RUN apk add --no-cache ca-certificates

# Unprivileged: the gateway needs no root and binds an unprivileged port.
RUN adduser -D -u 10001 switchyard
USER switchyard

COPY --from=build /out/switchyard /usr/local/bin/switchyard

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/switchyard"]
