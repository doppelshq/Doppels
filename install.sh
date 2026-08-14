#!/usr/bin/env bash
# Install doppels CLI from GitHub Releases.
# Usage:
#   curl -fsSL https://doppels.so/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/doppelshq/doppels/main/install.sh | sh
#   DOPPELS_VERSION=v0.0.0-dev.0 sh install.sh
set -euo pipefail

REPO="${DOPPELS_REPO:-doppelshq/doppels}"
VERSION="${DOPPELS_VERSION:-}"
INSTALL_DIR="${DOPPELS_INSTALL_DIR:-${HOME}/.local/bin}"
TMPDIR="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "${TMPDIR%/}/doppels-install.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "install.sh: need $1 on PATH" >&2
    exit 1
  }
}

need curl
need tar
need uname

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *)
    echo "install.sh: unsupported arch: $arch" >&2
    exit 1
    ;;
esac
case "$os" in
  linux | darwin) ;;
  *)
    echo "install.sh: unsupported OS: $os" >&2
    exit 1
    ;;
esac

resolve_version() {
  if [ -n "$VERSION" ]; then
    printf '%s\n' "$VERSION"
    return
  fi
  # Prefer latest non-prerelease; fall back to newest release (incl. prerelease).
  local tag
  tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
  if [ -z "$tag" ]; then
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases" \
      | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
  fi
  if [ -z "$tag" ]; then
    echo "install.sh: no GitHub Release found for ${REPO}" >&2
    exit 1
  fi
  printf '%s\n' "$tag"
}

VERSION="$(resolve_version)"
# Archive name uses version without leading v (GoReleaser .Version).
VER="${VERSION#v}"
ASSET="doppels_${VER}_${os}_${arch}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

echo "install.sh: downloading ${ASSET}"
curl -fsSL "$URL" -o "${WORKDIR}/${ASSET}"
tar -xzf "${WORKDIR}/${ASSET}" -C "$WORKDIR"
if [ ! -f "${WORKDIR}/doppels" ]; then
  echo "install.sh: archive missing doppels binary" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
install -m 0755 "${WORKDIR}/doppels" "${INSTALL_DIR}/doppels"
echo "install.sh: installed ${INSTALL_DIR}/doppels (${VERSION})"
if ! command -v doppels >/dev/null 2>&1; then
  echo "install.sh: add to PATH: export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
fi
"${INSTALL_DIR}/doppels" --version || true
