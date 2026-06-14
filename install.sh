#!/bin/sh
# Install sesh: detect macOS/Linux and amd64/arm64, download the latest
# release binary, verify its SHA-256 checksum, and let sesh finish its own
# install (-install copies it to ~/.local/bin and scaffolds ~/.sesh). Usage:
#
#   curl -fsSL https://raw.githubusercontent.com/mike-diff/sesh/main/install.sh | sh
#
set -eu

os=$(uname -s)
case "$os" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*)
    echo "unsupported OS: $os (sesh releases cover macOS and Linux)" >&2
    exit 1
    ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*)
    echo "unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

base="https://github.com/mike-diff/sesh/releases/latest/download"
asset="sesh-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $base/$asset"
curl -fsSL -o "$tmp/sesh" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"

# Verify the download against the published checksum before running it.
expected=$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/SHA256SUMS")
if [ -z "$expected" ]; then
    echo "no checksum for $asset in SHA256SUMS; aborting" >&2
    exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/sesh" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmp/sesh" | awk '{print $1}')
else
    echo "cannot verify checksum: need sha256sum or shasum on PATH" >&2
    exit 1
fi
if [ "$expected" != "$actual" ]; then
    echo "checksum mismatch for $asset (corrupt or tampered download); aborting" >&2
    echo "  expected $expected" >&2
    echo "  actual   $actual" >&2
    exit 1
fi

chmod +x "$tmp/sesh"
exec "$tmp/sesh" -install
