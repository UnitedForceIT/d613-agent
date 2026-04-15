#!/usr/bin/env bash
# D613 Labs Remote Agent — one-liner installer for macOS and Linux
#
# Usage (run this on the machine you want to access remotely):
#   curl -fsSL https://github.com/d613labs/d613-agent/releases/latest/download/install.sh | bash
#
# The script detects your OS/arch, downloads the right binary, and starts the
# agent.  No account, no configuration, no installation required.
set -euo pipefail

REPO="UntiedForce/d613-agent"
BINARY="d613-agent"
DEST="/tmp/${BINARY}"

# ── Detect OS ─────────────────────────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin) OS="darwin" ;;
  linux)  OS="linux"  ;;
  *)
    echo "Unsupported OS: $OS"
    exit 1
    ;;
esac

# ── Detect arch ───────────────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)        ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

FILENAME="${BINARY}-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${FILENAME}"

echo ""
echo "  D613 Labs Remote Agent"
echo "  ─────────────────────────────────────────"
echo "  Platform : ${OS}/${ARCH}"
echo "  Downloading ${FILENAME} ..."
echo ""

curl -fsSL "$URL" -o "$DEST"
chmod +x "$DEST"

echo "  Download complete.  Starting agent..."
echo ""

exec "$DEST" "$@"
