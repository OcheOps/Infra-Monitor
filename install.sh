#!/bin/sh
# Install or upgrade infra-monitor from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/OcheOps/Infra-Monitor/main/install.sh | sh
#
# Options (environment variables):
#   INFRA_MONITOR_VERSION      version to install, e.g. v0.1.0 (default: latest)
#   INFRA_MONITOR_INSTALL_DIR  where to put the binary (default: /usr/local/bin,
#                              or ~/.local/bin when that isn't writable and sudo
#                              isn't available)
set -eu

REPO="OcheOps/Infra-Monitor"
BASE_URL="${INFRA_MONITOR_BASE_URL:-https://github.com/$REPO/releases}"
VERSION="${INFRA_MONITOR_VERSION:-latest}"
INSTALL_DIR="${INFRA_MONITOR_INSTALL_DIR:-}"

info() { printf '%s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

fetch() { # fetch URL OUTPUT
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1"
  else
    fail "curl or wget is required"
  fi
}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s); download a build from $BASE_URL" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m); download a build from $BASE_URL" ;;
esac

if [ "$VERSION" = latest ]; then
  # Follow the /latest redirect instead of the API, which is rate-limited.
  if command -v curl >/dev/null 2>&1; then
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$BASE_URL/latest") || fail "could not look up the latest release"
  else
    url=$(wget -q -S --spider "$BASE_URL/latest" 2>&1 | sed -n 's/^ *Location: *//p' | tail -1 | tr -d '\r')
  fi
  VERSION="${url##*/}"
  case "$VERSION" in v[0-9]*) ;; *) fail "no release found at $BASE_URL" ;; esac
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac
number="${VERSION#v}"
archive="infra-monitor_${number}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "Downloading infra-monitor $VERSION for $os/$arch..."
fetch "$BASE_URL/download/$VERSION/$archive" "$tmp/$archive" || fail "download failed: $BASE_URL/download/$VERSION/$archive"
fetch "$BASE_URL/download/$VERSION/checksums.txt" "$tmp/checksums.txt" || fail "could not download checksums.txt"

expected=$(awk -v f="$archive" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
fi
[ "$expected" = "$actual" ] || fail "checksum mismatch for $archive"
info "Checksum verified."

tar -xzf "$tmp/$archive" -C "$tmp" infra-monitor

# sudo works without a password, or there's a terminal to ask for one.
can_sudo() {
  command -v sudo >/dev/null 2>&1 || return 1
  sudo -n true 2>/dev/null && return 0
  (: </dev/tty) 2>/dev/null
}

user_dir="$HOME/.local/bin"
sudo=""
if [ -z "$INSTALL_DIR" ]; then
  INSTALL_DIR=/usr/local/bin
  if [ ! -w "$INSTALL_DIR" ] && [ "$(id -u)" != 0 ]; then
    if can_sudo; then
      sudo=sudo
      info "Installing to $INSTALL_DIR (needs sudo)..."
    else
      INSTALL_DIR="$user_dir"
    fi
  fi
fi
if ! { $sudo mkdir -p "$INSTALL_DIR" && $sudo install -m 0755 "$tmp/infra-monitor" "$INSTALL_DIR/infra-monitor"; }; then
  [ -n "$sudo" ] || fail "could not write to $INSTALL_DIR"
  info "sudo failed; installing to $user_dir instead."
  INSTALL_DIR="$user_dir"
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$tmp/infra-monitor" "$INSTALL_DIR/infra-monitor"
fi

info ""
info "Installed $("$INSTALL_DIR/infra-monitor" version) to $INSTALL_DIR/infra-monitor"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) info "Add $INSTALL_DIR to your PATH, e.g.: echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.profile" ;;
esac

info ""
if command -v docker >/dev/null 2>&1 || command -v kubectl >/dev/null 2>&1; then
  info "Next: run \`infra-monitor\` to set up, or \`infra-monitor doctor\` to check prerequisites."
else
  info "Next: install Docker (https://docs.docker.com/engine/install/) or kubectl,"
  info "then run \`infra-monitor\` to set up. \`infra-monitor doctor\` checks what's missing."
fi
