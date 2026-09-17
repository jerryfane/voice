#!/usr/bin/env sh
set -eu

repo=${VOICE_REPO:-jerryfane/voice}
version=${VOICE_VERSION:-latest}
bin_dir=${VOICE_BIN_DIR:-"$HOME/.local/bin"}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac

if [ "$version" = latest ]; then
  url="https://github.com/$repo/releases/latest/download/voice_${os}_${arch}.tar.gz"
else
  url="https://github.com/$repo/releases/download/$version/voice_${os}_${arch}.tar.gz"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
curl -fL "$url" -o "$tmp/voice.tar.gz"
tar -xzf "$tmp/voice.tar.gz" -C "$tmp"
mkdir -p "$bin_dir"
install -m 0755 "$tmp/voice" "$bin_dir/voice"
echo "Installed $bin_dir/voice"
"$bin_dir/voice" version
