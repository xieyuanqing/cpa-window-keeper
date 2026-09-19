#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
CACHE="${HOME}/.cache/cpa-window-keeper-go"
# Keep CPU-heavy builds away from the production gateways.
CPU="${BUILD_CPU:-4}"
docker run --rm --network none --cpuset-cpus "$CPU" \
  -e GOMAXPROCS=1 -e GOPROXY=off -e GOSUMDB=off \
  -v "$ROOT:/src" -v "${HOME}/go/pkg/mod:/go/pkg/mod:ro" \
  -v "$CACHE:/root/.cache/go-build" -w /src golang:1.26-bookworm \
  sh -ec 'go test -p 1 -race -count=1 ./...; go vet ./...; mkdir -p dist; go build -p 1 -trimpath -buildmode=c-shared -ldflags="-s -w" -o dist/cpa-window-keeper-v0.1.5.so .'
sha256sum dist/cpa-window-keeper-v0.1.5.so
