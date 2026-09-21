#!/bin/sh
# fornax installer — fetches the latest GitHub release binary for this
# platform, verifies it against the release's sha256sums.txt, and puts it on
# PATH. Usage: curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
set -eu

REPO=earshot-run/fornax
DEST="${FORNAX_INSTALL:-$HOME/.local/bin}"

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

# gh reaches private repos; anonymous curl works once the repo is public.
# FORNAX_TAG / FORNAX_BASE override the lookup — used by the script's test.
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
	TAG="${FORNAX_TAG:-$(gh release view --repo "$REPO" --json tagName --jq .tagName)}"
	echo "downloading $ASSET ($TAG)"
	gh release download "$TAG" --repo "$REPO" -p "$ASSET" -p sha256sums.txt --dir "$TMP" --clobber
else
	need() { command -v "$1" >/dev/null 2>&1 || { echo "install needs $1" >&2; exit 1; }; }
	need curl
	TAG="${FORNAX_TAG:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)}"
	if [ -z "$TAG" ]; then
		echo "no fornax release reachable — the repo may be private (try \`gh auth login\`)," >&2
		echo "or build from source: go install github.com/$REPO@latest" >&2
		exit 1
	fi
	BASE="${FORNAX_BASE:-https://github.com/$REPO/releases/download/$TAG}"
	echo "downloading $ASSET ($TAG)"
	curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
	curl -fsSL "$BASE/sha256sums.txt" -o "$TMP/sha256sums.txt"
fi

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
