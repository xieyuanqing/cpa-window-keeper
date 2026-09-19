#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
CACHE="${HOME}/.cache/cpa-window-keeper-go"
# Keep CPU-heavy builds away from the production gateways.
CPU="${BUILD_CPU:-4}"
# The artifact name carries the version, so read it from the single source of truth.
VERSION="$(grep -oP 'const version = "\K[^"]+' config.go)"
ARTIFACT="dist/cpa-window-keeper-v${VERSION}.so"
test -n "$VERSION"
test ! -e "$ARTIFACT"
docker run --rm --network none --cpuset-cpus "$CPU" \
  -e GOMAXPROCS=1 -e GOPROXY=off -e GOSUMDB=off \
  -v "$ROOT:/src" -v "${HOME}/go/pkg/mod:/go/pkg/mod:ro" \
  -v "$CACHE:/root/.cache/go-build" -w /src golang:1.26-bookworm \
  sh -ec "go test -p 1 -race -count=1 ./...; go vet ./...; mkdir -p dist; go build -p 1 -trimpath -buildmode=c-shared -ldflags=\"-s -w\" -o ${ARTIFACT} ."
sha256sum "${ARTIFACT}" > "${ARTIFACT}.sha256"
cat "${ARTIFACT}.sha256"
