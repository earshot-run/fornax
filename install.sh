#!/bin/sh
# fornax installer — fetches the latest successful main build for this
# platform, verifies it against the release's sha256sums.txt, and puts it on
# PATH. Usage: curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
set -eu

REPO=earshot-run/fornax
DEST="${FORNAX_INSTALL:-$HOME/.local/bin}"
TAG="${FORNAX_TAG:-}"
RELEASES="${FORNAX_RELEASES:-https://github.com/$REPO/releases/download}"

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

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

USE_GH=false
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
	USE_GH=true
else
	command -v curl >/dev/null 2>&1 || { echo "install needs curl" >&2; exit 1; }
fi

# Resolve once so the binary and checksum still match during a channel update.
if [ -z "$TAG" ]; then
	if [ "$USE_GH" = true ]; then
		TAG=$(gh release download main-build --repo "$REPO" -p version.txt -O -)
	else
		TAG=$(curl -fsSL --retry 3 -H 'Cache-Control: no-cache' "$RELEASES/main-build/version.txt?check=$(date +%s)")
	fi
	SHA=${TAG#main-}
	case "$SHA" in
	"$TAG"|*[!0-9a-f]*) echo "invalid main build version: $TAG" >&2; exit 1 ;;
	esac
	[ "${#SHA}" -eq 40 ] || { echo "invalid main build version: $TAG" >&2; exit 1; }
fi

echo "downloading $ASSET ($TAG)"
if [ "$USE_GH" = true ]; then
	gh release download "$TAG" --repo "$REPO" -p "$ASSET" -p sha256sums.txt --dir "$TMP" --clobber
else
	BASE="${FORNAX_BASE:-$RELEASES/$TAG}"
	curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
	curl -fsSL "$BASE/sha256sums.txt" -o "$TMP/sha256sums.txt"
fi

cd "$TMP"
if command -v sha256sum >/dev/null 2>&1; then
	awk -v asset="$ASSET" '$2 == asset { print }' sha256sums.txt | sha256sum -c - >/dev/null
else
	# macOS ships shasum, not sha256sum
	awk -v asset="$ASSET" '$2 == asset { print }' sha256sums.txt | shasum -a 256 -c - >/dev/null
fi

mkdir -p "$DEST"
mv "$TMP/$ASSET" "$DEST/fornax"
chmod 755 "$DEST/fornax"
echo "installed $DEST/fornax ($TAG)"
case ":$PATH:" in
*":$DEST:"*) ;;
*) echo "note: $DEST is not on PATH — add it, or run: export PATH=\"$DEST:\$PATH\"" ;;
esac
