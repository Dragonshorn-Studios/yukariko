#!/usr/bin/env sh
# One-command installer for Yukariko release builds (Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/Dragonshorn-Studios/yukariko/main/scripts/install.sh | sudo sh
#
# Run as root it also provisions the system layout the daemon's root
# defaults expect: a dedicated `yukariko` user, /etc/yukariko/yukariko.yaml
# (created only when absent, never overwritten), /var/lib/yukariko, and the
# systemd unit from the release assets.
#
# Options (when run from a checkout instead of piped):
#   scripts/install.sh [--to DIR] [--version vX.Y.Z] [--docker-group] [--no-system-setup]
#                      [--read-write DIR]... [--home-read]
#
# --read-write adds DIR to the systemd unit's ReadWritePaths via a drop-in
# (repeatable). The shipped unit is deliberately strict: only /var/lib/yukariko
# is writable, which blocks git-source apps — their fetch and ff-only merge
# write to the worktree. Every git app's worktree must be exposed this way.
# --home-read relaxes ProtectHome to read-only for configs living under /home.
#
# --docker-group grants the yukariko user docker-group membership. Docker
# access is root-equivalent privilege (docs/docker-access.md); without the
# flag the command is printed instead of run.
#
# GITHUB_TOKEN is used for the latest-release lookup if set (rate limits).
# YUKARIKO_API_URL / YUKARIKO_DOWNLOAD_URL override the endpoints and
# YUKARIKO_SYS_USER / _CONF_DIR / _DATA_DIR / _UNIT the system layout
# (mirrors, prefix installs, tests).
set -eu

REPO="Dragonshorn-Studios/yukariko"
API="${YUKARIKO_API_URL:-https://api.github.com/repos/${REPO}}"
DL="${YUKARIKO_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download}"
BIN_DIR="/usr/local/bin"
SYS_USER="${YUKARIKO_SYS_USER:-yukariko}"
SYS_CONF_DIR="${YUKARIKO_SYS_CONF_DIR:-/etc/yukariko}"
SYS_CONF="${SYS_CONF_DIR}/yukariko.yaml"
SYS_DATA_DIR="${YUKARIKO_SYS_DATA_DIR:-/var/lib/yukariko}"
SYS_UNIT="${YUKARIKO_SYS_UNIT:-/etc/systemd/system/yukariko.service}"
VERSION=""
DOCKER_GROUP=0
SYSTEM_SETUP=1
RW_PATHS=""
HOME_READ=0

usage() {
  echo "usage: install.sh [--to DIR] [--version vX.Y.Z] [--docker-group] [--no-system-setup] [--read-write DIR]... [--home-read]" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --to) [ "$#" -ge 2 ] || usage; BIN_DIR="$2"; shift 2 ;;
    --version) [ "$#" -ge 2 ] || usage; VERSION="$2"; shift 2 ;;
    --docker-group) DOCKER_GROUP=1; shift ;;
    --no-system-setup) SYSTEM_SETUP=0; shift ;;
    --read-write) [ "$#" -ge 2 ] || usage; RW_PATHS="${RW_PATHS}${RW_PATHS:+
}$2"; shift 2 ;;
    --home-read) HOME_READ=1; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

fail() {
  echo "install: $*" >&2
  exit 1
}

note() {
  echo "install: $*"
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
# Checksums cover every artifact; verify only the files we fetch.
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
note "installed $("${BIN_DIR}/yukariko" --version) to ${BIN_DIR}/yukariko"

# --- system layout (root only) ------------------------------------------------

if [ "$SYSTEM_SETUP" -ne 1 ] || [ "$(id -u)" -ne 0 ]; then
  if [ -n "${RW_PATHS}" ] || [ "$HOME_READ" -eq 1 ]; then
    note "warning: --read-write/--home-read need the root system setup; ignored"
  fi
  if [ "$(uname -s)" = "Linux" ] && [ "$SYSTEM_SETUP" -eq 1 ]; then
    note "rerun as root (or pipe to sudo sh) to also provision the ${SYS_USER} user, ${SYS_CONF_DIR}, ${SYS_DATA_DIR}, and the systemd unit."
  fi
  exit 0
fi

if ! command -v useradd >/dev/null 2>&1; then
  note "useradd not found; skipping system setup (create the ${SYS_USER} user and directories per docs/operations.md)"
  exit 0
fi

if id "${SYS_USER}" >/dev/null 2>&1; then
  note "user ${SYS_USER} already exists"
else
  SHELL_NOLOGIN=""
  for s in /usr/sbin/nologin /sbin/nologin /bin/false; do
    [ -x "$s" ] && SHELL_NOLOGIN="$s" && break
  done
  useradd --system --home-dir "${SYS_DATA_DIR}" --shell "${SHELL_NOLOGIN}" "${SYS_USER}" \
    || fail "could not create the ${SYS_USER} system user"
  note "created system user ${SYS_USER}"
fi

# Configuration: never overwrite an operator's file.
mkdir -p "${SYS_CONF_DIR}"
chown root:"${SYS_USER}" "${SYS_CONF_DIR}"
chmod 0750 "${SYS_CONF_DIR}"
if [ -f "${SYS_CONF}" ]; then
  note "config ${SYS_CONF} already exists; left untouched"
else
  cat > "${SYS_CONF}" <<'EOF'
# Yukariko daemon configuration (provisioned by scripts/install.sh).
# Schema reference: https://github.com/Dragonshorn-Studios/yukariko/blob/main/docs/config.md
# Discover running containers into this file:  sudo yukariko learn
schema_version: 1
apps: []
EOF
  chown root:"${SYS_USER}" "${SYS_CONF}"
  chmod 0640 "${SYS_CONF}"
  note "wrote empty config ${SYS_CONF} (fill it with: sudo yukariko learn)"
fi

# Data directory matches the unit's ReadWritePaths/NoExecPaths. The
# recursive chown heals store files a root CLI pass may have created as
# root-owned before the service ever started.
mkdir -p "${SYS_DATA_DIR}"
chown -R "${SYS_USER}":"${SYS_USER}" "${SYS_DATA_DIR}"
chmod 0750 "${SYS_DATA_DIR}"
note "data dir ${SYS_DATA_DIR} ready"

# Docker group membership is a deliberate operator decision: it is
# root-equivalent privilege (docs/docker-access.md).
if [ "$DOCKER_GROUP" -eq 1 ]; then
  if getent group docker >/dev/null 2>&1; then
    usermod -aG docker "${SYS_USER}" 2>/dev/null || gpasswd -a "${SYS_USER}" docker
    note "added ${SYS_USER} to the docker group (root-equivalent privilege; see docs/docker-access.md)"
  else
    note "docker group not present; install Docker, then run: adduser ${SYS_USER} docker"
  fi
else
  note "docker access not granted. The daemon needs it; when ready: adduser ${SYS_USER} docker"
  note "(docker group membership is root-equivalent privilege - docs/docker-access.md)"
fi

# systemd unit from the release assets, checksum-verified like the binary.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  if curl -fsSL -o "${TMP}/yukariko.service" "${DL}/${VERSION}/yukariko.service"; then
    (cd "${TMP}" && grep -F "yukariko.service" SHA256SUMS | sha256sum -c -) \
      || fail "checksum mismatch for yukariko.service"
    mkdir -p "$(dirname "${SYS_UNIT}")"
    install -m 0644 "${TMP}/yukariko.service" "${SYS_UNIT}"
    # Git-source worktrees must be exposed to the strict unit; drop-in list
    # directives append, so the data-dir ReadWritePaths entry stays.
    if [ -n "${RW_PATHS}" ] || [ "$HOME_READ" -eq 1 ]; then
      mkdir -p "${SYS_UNIT}.d"
      {
        echo "# Generated by scripts/install.sh (--read-write/--home-read)."
        echo "[Service]"
        printf '%s\n' "${RW_PATHS}" | while IFS= read -r p; do
          [ -n "$p" ] || continue
          echo "ReadWritePaths=$p"
        done
        if [ "$HOME_READ" -eq 1 ]; then
          echo "ProtectHome=read-only"
        fi
      } > "${SYS_UNIT}.d/10-writables.conf"
      note "wrote ${SYS_UNIT}.d/10-writables.conf"
    fi
    systemctl daemon-reload
    note "installed systemd unit ${SYS_UNIT} (not enabled)"
    [ "${BIN_DIR}" = "/usr/local/bin" ] \
      || note "warning: the unit expects /usr/local/bin/yukariko but the binary went to ${BIN_DIR}"
  else
    note "could not download yukariko.service from ${VERSION} (asset missing or network); unit skipped"
  fi
else
  note "systemd not detected; unit skipped"
fi

if [ -n "${RW_PATHS}" ] && { ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; }; then
  note "warning: --read-write paths were requested but no systemd unit was installed"
fi

# Rootless Docker discovery: sockets under /run/user/<uid>/ suggest their own
# exposure. Never granted automatically - the operator runs the command.
if [ "$(id -u)" -eq 0 ] && [ -d /run/systemd/system ]; then
  FOUND_SOCKS=""
  for SOCK in /run/user/[0-9]*/docker.sock; do
    [ -S "$SOCK" ] && FOUND_SOCKS="${FOUND_SOCKS}${FOUND_SOCKS:+ }$SOCK"
  done
  if [ -n "$FOUND_SOCKS" ]; then
    echo ""
    note "rootless Docker socket(s) detected:"
    for SOCK in $FOUND_SOCKS; do
      SOCK_UID="${SOCK#/run/user/}"
      SOCK_UID="${SOCK_UID%%/*}"
      SOCK_USER=""
      if command -v getent >/dev/null 2>&1; then
        SOCK_USER="$(getent passwd "${SOCK_UID}" 2>/dev/null | cut -d: -f1)"
      fi
      echo "  ${SOCK}  (user: ${SOCK_USER:-uid ${SOCK_UID}})"
    done
    echo "  to manage the daemon's apps with Yukariko:"
    echo "    sudo yukariko expose /run/user/<uid>/docker.sock"
    echo "    and point the config at it:  docker: {host: unix:///run/user/<uid>/docker.sock}"
  fi
fi

echo ""
echo "next steps:"
echo "  sudo yukariko learn                  # scan Docker into ${SYS_CONF}"
if [ -z "${RW_PATHS}" ]; then
  echo "  git-source apps: expose their worktrees to the strict unit first:"
  echo "    sudo yukariko expose               # from inside the project directory"
  echo "                                          (or rerun the installer with --read-write <dir>)"
fi
echo "  sudo systemctl enable --now yukariko # start the daemon"
