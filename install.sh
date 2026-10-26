#!/bin/sh
# Install telegram-mcp from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/itskamol/telegram-mcp-go/main/install.sh | sh
#
# Environment:
#   VERSION=v0.2.0       install this release instead of the latest
#   BIN_DIR=/usr/local/bin   install here instead of ~/.local/bin
set -eu

REPO="itskamol/telegram-mcp-go"
PROJECT="telegram-mcp-go"
BIN="telegram-mcp"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

need curl
need tar
need uname

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS $(uname -s); on Windows download the zip from https://github.com/$REPO/releases" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

if [ -z "${VERSION:-}" ]; then
  # /releases/latest redirects to /releases/tag/<tag>; no API token needed.
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/##')
  case "$VERSION" in v*) ;; *) die "could not find the latest release" ;; esac
fi

archive="${PROJECT}_${VERSION#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading $BIN $VERSION for $os/$arch..."
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "checksums.txt download failed"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
else
  got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
fi
[ "$got" = "$want" ] || die "checksum mismatch for $archive (got $got, want $want)"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/$BIN" "$BIN_DIR/$BIN"
say "Installed $BIN_DIR/$BIN ($("$BIN_DIR/$BIN" version))"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "Note: $BIN_DIR is not on your PATH. Add it, e.g.: echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc" ;;
esac

say "
Next steps:
  1. Get api_id and api_hash at https://my.telegram.org/apps and put them in
     ~/.config/telegram-mcp/config.toml:
       api_id = 1234567
       api_hash = \"...\"
  2. $BIN login --qr
  3. $BIN install claude-desktop   (or: $BIN install claude-code)"
