# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY static/ ./static
COPY templates/ ./templates
COPY migrations/ ./migrations
COPY schema.sql ./

# The SQLite driver is pure Go, so this is a static build with no C toolchain.
RUN CGO_ENABLED=0 go build -trimpath -o /out/statusnook \
    -ldflags "-w -s \
    -X main.CA=https://acme-v02.api.letsencrypt.org/directory \
    -X main.BUILD=release"


FROM alpine:3.20

# ca-certificates: the GitHub API and every https monitor need a trust store.
# The previous image shipped without it and also copied the whole build tree,
# including the Go toolchain and sources, into the runtime layer.
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 10001 -S statusnook && \
    adduser -u 10001 -S -G statusnook statusnook

WORKDIR /app

COPY --from=build /out/statusnook /app/statusnook

# The data directory holds the database, the key that decrypts config secrets
# and any managed certificates. Mount a volume here to keep them across
# container recreation.
RUN mkdir -p /app/statusnook-data && \
    chown -R statusnook:statusnook /app

USER statusnook:statusnook

ENV STATUSNOOK_PORT=8000 \
    STATUSNOOK_DATA_DIR=/app/statusnook-data

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${STATUSNOOK_PORT}/healthz" || exit 1

ENTRYPOINT ["/app/statusnook", "-docker"]
