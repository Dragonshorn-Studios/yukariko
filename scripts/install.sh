#!/usr/bin/env sh
# One-command installer for Yukariko release builds (Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/Dragonshorn-Studios/yukariko/main/scripts/install.sh | sudo sh
#
# When run from a checkout instead of piped, it accepts options:
#   scripts/install.sh [--to DIR] [--version vX.Y.Z]
#
# GITHUB_TOKEN is used for the latest-release lookup if set (rate limits).
# YUKARIKO_API_URL / YUKARIKO_DOWNLOAD_URL override the endpoints (mirrors,
# tests).
set -eu

REPO="Dragonshorn-Studios/yukariko"
API="${YUKARIKO_API_URL:-https://api.github.com/repos/${REPO}}"
DL="${YUKARIKO_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download}"
BIN_DIR="/usr/local/bin"
VERSION=""

usage() {
  echo "usage: install.sh [--to DIR] [--version vX.Y.Z]" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --to) [ "$#" -ge 2 ] || usage; BIN_DIR="$2"; shift 2 ;;
    --version) [ "$#" -ge 2 ] || usage; VERSION="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

fail() {
  echo "install: $*" >&2
  exit 1
}

[ "$(uname -s)" = "Linux" ] || fail "release binaries target Linux (this is $(uname -s))"
case "$(uname -m)" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) fail "no release binaries for $(uname -m)" ;;
esac

for CMD in curl tar sha256sum install; do
  command -v "$CMD" >/dev/null 2>&1 || fail "missing required command: $CMD"
done

if [ -z "$VERSION" ]; then
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    VERSION="$(curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" \
      "${API}/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  else
    VERSION="$(curl -fsSL "${API}/releases/latest" \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  fi
fi
[ -n "$VERSION" ] || fail "could not resolve the latest release (rate limit? set GITHUB_TOKEN or pass --version)"
case "$VERSION" in
  v*) ;;
  *) fail "resolved version '${VERSION}' does not look like a release tag (v*)" ;;
esac

TARBALL="yukariko-${VERSION}-linux-${ARCH}.tar.gz"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading ${TARBALL}"
curl -fsSL -o "${TMP}/${TARBALL}" "${DL}/${VERSION}/${TARBALL}"
curl -fsSL -o "${TMP}/SHA256SUMS" "${DL}/${VERSION}/SHA256SUMS"
# Checksums cover every arch; verify only the file we fetched.
(cd "${TMP}" && grep -F "${TARBALL}" SHA256SUMS | sha256sum -c -) \
  || fail "checksum mismatch for ${TARBALL}"
tar -xzf "${TMP}/${TARBALL}" -C "${TMP}"

BIN="${TMP}/yukariko-${VERSION}-linux-${ARCH}"
[ -f "${BIN}" ] || fail "tarball did not contain the expected binary"

mkdir -p "${BIN_DIR}" 2>/dev/null || true
if [ -w "${BIN_DIR}" ] 2>/dev/null; then
  install -m 0755 "${BIN}" "${BIN_DIR}/yukariko"
elif command -v sudo >/dev/null 2>&1; then
  sudo mkdir -p "${BIN_DIR}"
  sudo install -m 0755 "${BIN}" "${BIN_DIR}/yukariko"
else
  fail "cannot write to ${BIN_DIR} - rerun as root or pass --to <dir>"
fi

echo "installed $("${BIN_DIR}/yukariko" --version) to ${BIN_DIR}/yukariko"
