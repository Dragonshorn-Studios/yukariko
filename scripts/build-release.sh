#!/usr/bin/env sh
# Reproducible release build for Yukariko's documented Linux targets.
# Usage: scripts/build-release.sh <version> [outdir] [target ...]
#   Explicit os/arch targets (e.g. linux/arm64) build exactly those and skip
#   the host binary; with no targets the documented defaults build plus a
#   host development binary.
# Produces <outdir>/yukariko-<version>-<os>-<arch>[.exe] plus SHA-256 checksums.
set -eu

VERSION="${1:?usage: build-release.sh <version> [outdir] [target ...]}"
OUT="${2:-dist}"
if [ "$#" -ge 2 ]; then
  shift 2
else
  shift 1
fi
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(git log -1 --format=%cI 2>/dev/null || echo 1970-01-01T00:00:00Z)"
LDFLAGS="-s -w -buildid= -X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=${VERSION} -X github.com/Dragonshorn-Studios/yukariko/internal/version.Commit=${COMMIT} -X github.com/Dragonshorn-Studios/yukariko/internal/version.Date=${DATE}"

mkdir -p "${OUT}"

build_target() {
  OS="${1%%/*}"
  ARCH="${1##*/}"
  BIN="${OUT}/yukariko-${VERSION}-${OS}-${ARCH}"
  echo "building ${BIN}"
  CGO_ENABLED=0 GOOS="${OS}" GOARCH="${ARCH}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${BIN}" ./cmd/yukariko
}

if [ "$#" -gt 0 ]; then
  for TARGET in "$@"; do
    case "${TARGET}" in
      */*) build_target "${TARGET}" ;;
      *) echo "invalid target '${TARGET}' (want os/arch)" >&2; exit 1 ;;
    esac
  done
else
  for TARGET in linux/amd64 linux/arm64; do
    build_target "${TARGET}"
  done

  # Local development binary for the host platform (Windows gets .exe).
  HOST="${OUT}/yukariko-${VERSION}-host"
  if [ "${GOOS:-}" = "windows" ] || (uname -s | grep -qi windows); then
    HOST="${HOST}.exe"
  fi
  echo "building ${HOST}"
  go build -trimpath -ldflags "${LDFLAGS}" -o "${HOST}" ./cmd/yukariko
fi

# Checksums for every artifact.
(cd "${OUT}" && sha256sum yukariko-"${VERSION}"* > SHA256SUMS)
echo "artifacts in ${OUT}:"
cat "${OUT}/SHA256SUMS"
