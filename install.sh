#!/usr/bin/env bash
# Install doppels CLI from GitHub Releases (macOS, Linux, Windows via WSL2).
# Usage:
#   curl -fsSL https://doppels.so/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/doppelshq/doppels/main/install.sh | sh
#   DOPPELS_VERSION=v0.1.0-alpha.1 sh install.sh
# Windows: install WSL2, open a Linux shell, then run the curl line above.
# Native Windows (.exe / PowerShell) is not supported in alpha.
set -euo pipefail

REPO="${DOPPELS_REPO:-doppelshq/doppels}"
VERSION="${DOPPELS_VERSION:-}"
INSTALL_DIR="${DOPPELS_INSTALL_DIR:-${HOME}/.local/bin}"
TMPDIR="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "${TMPDIR%/}/doppels-install.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  DIM=$'\033[2m'
  BOLD=$'\033[1m'
  GREEN=$'\033[32m'
  CYAN=$'\033[36m'
  RESET=$'\033[0m'
else
  DIM=''
  BOLD=''
  GREEN=''
  CYAN=''
  RESET=''
fi

say() {
  printf '%b\n' "$*"
}

field() {
  printf '  %s%-10s%s  ' "$DIM" "$1" "$RESET"
}

fail() {
  say ""
  say "${BOLD}Install failed${RESET}  $*"
  say "${DIM}Need help? https://doppels.so${RESET}"
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "need $1 on PATH"
}

need curl
need tar
need uname

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) fail "unsupported arch: $arch (need amd64 or arm64)" ;;
esac
case "$os" in
  linux | darwin) ;;
  mingw* | msys* | cygwin*)
    fail "native Windows is not supported — use WSL2, then: curl -fsSL https://doppels.so/install.sh | sh"
    ;;
  *)
    fail "unsupported OS: $os (need linux, darwin, or Windows via WSL2)"
    ;;
esac

resolve_version() {
  if [ -n "$VERSION" ]; then
    printf '%s\n' "$VERSION"
    return
  fi
  local tag
  tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
  if [ -z "$tag" ]; then
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases" \
      | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
  fi
  if [ -z "$tag" ]; then
    fail "no GitHub Release found for ${REPO}"
  fi
  printf '%s\n' "$tag"
}

TARGET="${INSTALL_DIR}/doppels"
PREVIOUS=""
if [ -x "$TARGET" ]; then
  PREVIOUS="$("$TARGET" --version 2>/dev/null | awk 'NF {print $2; exit}' || true)"
fi

say ""
say "${BOLD}Doppels${RESET}  ${DIM}local-first execution control plane${RESET}"
say ""

VERSION="$(resolve_version)"
VER="${VERSION#v}"
ASSET="doppels_${VER}_${os}_${arch}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

field "Platform"
say "${os}/${arch}"
field "Release"
say "${VERSION}"
field "Install"
say "${TARGET}"

if [ -n "$PREVIOUS" ]; then
  field "Update"
  if [ "$PREVIOUS" = "$VER" ] || [ "$PREVIOUS" = "$VERSION" ]; then
    say "${PREVIOUS} ${DIM}(reinstall)${RESET}"
  else
    say "${PREVIOUS} ${DIM}→${RESET} ${VERSION}"
  fi
fi

say ""
field "Fetch"
printf '%s' "${DIM}downloading ${ASSET}…${RESET}"
if [ -t 2 ]; then
  printf '\n'
  curl -#fSL "$URL" -o "${WORKDIR}/${ASSET}"
else
  printf '\n'
  curl -fsSL "$URL" -o "${WORKDIR}/${ASSET}"
fi

tar -xzf "${WORKDIR}/${ASSET}" -C "$WORKDIR"
if [ ! -f "${WORKDIR}/doppels" ]; then
  fail "archive missing doppels binary"
fi

mkdir -p "$INSTALL_DIR"
install -m 0755 "${WORKDIR}/doppels" "$TARGET"

INSTALLED="$("$TARGET" --version 2>/dev/null | awk 'NF {print $2; exit}' || true)"
if [ -z "$INSTALLED" ]; then
  INSTALLED="$VER"
fi

say ""
field "Ready"
say "${GREEN}${BOLD}doppels ${INSTALLED}${RESET}"

IN_PATH=false
if command -v doppels >/dev/null 2>&1; then
  IN_PATH=true
fi

say ""
say "${BOLD}Next${RESET}"
say "  ${CYAN}doppels init${RESET}     ${DIM}working tree + Space${RESET}"
say "  ${CYAN}doppels help${RESET}     ${DIM}commands and flags${RESET}"
say "  ${DIM}https://doppels.so${RESET}"

if [ "$IN_PATH" = false ]; then
  say ""
  say "${BOLD}PATH${RESET}"
  say "  ${DIM}Add to your shell profile:${RESET}"
  say "  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

say ""
