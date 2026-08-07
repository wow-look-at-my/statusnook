# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./

COPY static/ ./static

COPY migrations/ ./migrations

COPY schema.sql ./

RUN apk add --update gcc musl-dev

RUN CGO_ENABLED=1 go build -o statusnook \
    -ldflags "-w -s -X main.CA=https://acme-v02.api.letsencrypt.org/directory -X main.BUILD=release"


FROM alpine:3.21

WORKDIR /app

# Only the binary: the previous `COPY --from=0 /app ./` shipped the Go sources,
# go.mod and go.sum in the published image. static/, migrations/ and schema.sql
# are embedded in it.
COPY --from=build /app/statusnook ./statusnook

# statusnook-data holds the database; certmagic holds issued certificates.
# Neither was declared, so the normal upgrade -- rm the container, run the new
# image -- silently discarded the monitors, users, history and certificates.
VOLUME ["/app/statusnook-data", "/app/certmagic"]

RUN adduser -D -H -u 10001 statusnook \
    && chown statusnook /app
USER statusnook

ENV PORT=8000

EXPOSE $PORT

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:$PORT/healthz" >/dev/null || exit 1

CMD ["/bin/sh", "-c", "./statusnook -port $PORT -docker"]
