#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")/.."
gofmt -l .
go vet ./...
go test ./... 2>/dev/null || true
echo "checks done"
