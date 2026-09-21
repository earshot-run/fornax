#!/bin/sh
# fornax installer — fetches the latest GitHub release binary for this
# platform, verifies it against the release's sha256sums.txt, and puts it on
# PATH. Usage: curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
set -eu

REPO=earshot-run/fornax
DEST="${FORNAX_INSTALL:-$HOME/.local/bin}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "install needs $1" >&2; exit 1; }; }
need curl

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$OS" in
darwin|linux) ;;
*) echo "no installer for $OS — grab a binary from https://github.com/$REPO/releases" >&2; exit 1 ;;
esac
case "$ARCH" in
arm64|aarch64) ARCH=arm64 ;;
x86_64|amd64) ARCH=amd64 ;;
*) echo "no build for $ARCH" >&2; exit 1 ;;
esac
ASSET="fornax-$OS-$ARCH"

# FORNAX_TAG / FORNAX_BASE override the release lookup — used by the
# script's own test (file:// works through curl).
TAG="${FORNAX_TAG:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)}"
if [ -z "$TAG" ]; then
	echo "no fornax release yet — build from source: go install github.com/$REPO@latest" >&2
	exit 1
fi
BASE="${FORNAX_BASE:-https://github.com/$REPO/releases/download/$TAG}"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "downloading $ASSET ($TAG)"
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
curl -fsSL "$BASE/sha256sums.txt" -o "$TMP/sha256sums.txt"

cd "$TMP"
if command -v sha256sum >/dev/null 2>&1; then
	grep " $ASSET\$" sha256sums.txt | sha256sum -c - >/dev/null
else
	# macOS ships shasum, not sha256sum
	grep " $ASSET\$" sha256sums.txt | sed "s/ \($ASSET\)$/  \1/" | shasum -a 256 -c - >/dev/null
fi

mkdir -p "$DEST"
mv "$TMP/$ASSET" "$DEST/fornax"
chmod 755 "$DEST/fornax"
echo "installed $DEST/fornax ($TAG)"
case ":$PATH:" in
*":$DEST:"*) ;;
*) echo "note: $DEST is not on PATH — add it, or run: export PATH=\"$DEST:\$PATH\"" ;;
esac
