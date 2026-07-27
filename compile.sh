#!/bin/bash

# The SQLite driver is pure Go, so cross-compiling needs no C toolchain.
set -euo pipefail

VERSION=v0.3.0
LDFLAGS="-w -s -X main.CA=https://acme-v02.api.letsencrypt.org/directory -X main.BUILD=release"

for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "$LDFLAGS" -o "bin/statusnook_linux_${arch}_${VERSION}"
done
