#!/usr/bin/env bash
# CPU-pinned multi-platform build.
#
# Runs the native Linux amd64 race tests + vet, then emits a legacy .so for
# manual installs and store-compatible archives (one library at zip root):
#
#   dist/cpa-window-keeper_<version>_<goos>_<goarch>.zip
#   dist/checksums.txt
#
# Supported release platforms: linux/amd64 and linux/arm64.
# The state lock uses Linux flock semantics; Windows is intentionally not built.
# macOS also needs both an osxcross toolchain and a lock implementation.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
ID="cpa-window-keeper"
VERSION="$(grep -oP 'const version = "\K[^"]+' config.go)"
CACHE="${HOME}/.cache/cpa-window-keeper-go"
CPU="${BUILD_CPU:-4}"
ARTIFACT="dist/${ID}-v${VERSION}.so"
test -n "$VERSION"
mkdir -p "$CACHE" dist
rm -f "dist/${ID}_${VERSION}_"*.zip dist/checksums.txt \
      "$ARTIFACT" "$ARTIFACT.h" "${ARTIFACT}.sha256"

IMAGE="golang:1.26-bookworm"
PKGS="gcc-aarch64-linux-gnu zip"

echo "building $IMAGE → $ID v$VERSION (cpuset $CPU)"
docker run --rm --cpuset-cpus "$CPU" \
  -e GOMAXPROCS=1 -e GOPROXY=off -e GOSUMDB=off \
  -v "$ROOT:/src" -v "${HOME}/go/pkg/mod:/go/pkg/mod:ro" \
  -v "$CACHE:/root/.cache/go-build" -w /src "$IMAGE" \
  sh -c '
    set -e
    apt-get update -qq >/dev/null && apt-get install -y -qq '"$PKGS"' >/dev/null

    go test -p 1 -race -count=1 ./...
    go vet ./...

    build () { # goarch cc out
      local goarch="$1" cc="$2" out="$3"
      echo "-- linux/$goarch"
      if [ "$goarch" = "arm64" ]; then
        env -u GOOS -u GOARCH CC="$cc" CGO_ENABLED=1 GOOS=linux GOARCH="$goarch" \
          go build -p 1 -trimpath -buildmode=c-shared -ldflags="-s -w" -o "$out" .
      else
        env -u GOOS -u GOARCH -u CC CGO_ENABLED=1 GOOS=linux GOARCH="$goarch" \
          go build -p 1 -trimpath -buildmode=c-shared -ldflags="-s -w" -o "$out" .
      fi
    }

    build amd64 /usr/bin/gcc /src/dist/'"$ID"'.so
    rm -f /src/dist/*.h
    ( cd /src/dist && zip -q '"$ID"'_'"$VERSION"'_linux_amd64.zip '"$ID"'.so )
    mv /src/dist/'"$ID"'.so /src/'"$ARTIFACT"'

    build arm64 aarch64-linux-gnu-gcc /src/dist/'"$ID"'.so
    rm -f /src/dist/*.h
    ( cd /src/dist && zip -q '"$ID"'_'"$VERSION"'_linux_arm64.zip '"$ID"'.so && rm '"$ID"'.so )

    ( cd /src/dist && sha256sum '"$ID"'_'"$VERSION"'_*.zip > checksums.txt )
  '

sha256sum "$ARTIFACT" > "${ARTIFACT}.sha256"
cat "${ARTIFACT}.sha256"
echo "--- checksums.txt ---"
cat dist/checksums.txt
