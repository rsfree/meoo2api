#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/meoo2api ./cmd/server
echo "built bin/meoo2api"
