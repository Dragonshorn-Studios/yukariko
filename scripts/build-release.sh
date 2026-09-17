#!/usr/bin/env sh
# Reproducible release build for Yukariko's documented Linux targets.
# Usage: scripts/build-release.sh <version> [outdir]
# Produces dist/yukariko-<version>-<os>-<arch>[.exe] plus SHA-256 checksums.
set -eu

VERSION="${1:?usage: build-release.sh <version> [outdir]}"
OUT="${2:-dist}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(git log -1 --format=%cI 2>/dev/null || echo 1970-01-01T00:00:00Z)"
LDFLAGS="-s -w -buildid= -X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=${VERSION} -X github.com/Dragonshorn-Studios/yukariko/internal/version.Commit=${COMMIT} -X github.com/Dragonshorn-Studios/yukariko/internal/version.Date=${DATE}"

mkdir -p "${OUT}"
for TARGET in linux/amd64 linux/arm64; do
  OS="${TARGET%%/*}"
  ARCH="${TARGET##*/}"
  BIN="${OUT}/yukariko-${VERSION}-${OS}-${ARCH}"
  echo "building ${BIN}"
  CGO_ENABLED=0 GOOS="${OS}" GOARCH="${ARCH}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${BIN}" ./cmd/yukariko
done

# Local development binary for the host platform (Windows gets .exe).
HOST="${OUT}/yukariko-${VERSION}-host"
if [ "${GOOS:-}" = "windows" ] || (uname -s | grep -qi windows); then
  HOST="${HOST}.exe"
fi
echo "building ${HOST}"
go build -trimpath -ldflags "${LDFLAGS}" -o "${HOST}" ./cmd/yukariko

# Checksums for every artifact.
(cd "${OUT}" && sha256sum yukariko-"${VERSION}"* > SHA256SUMS)
echo "artifacts in ${OUT}:"
cat "${OUT}/SHA256SUMS"
