#!/usr/bin/env sh
set -eu

repo=${VOICED_REPO:-jerryfane/herdr-voice}
version=${VOICED_VERSION:-latest}
bin_dir=${VOICED_BIN_DIR:-"$HOME/.local/bin"}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac

if [ "$version" = latest ]; then
  url="https://github.com/$repo/releases/latest/download/voiced_${os}_${arch}.tar.gz"
else
  url="https://github.com/$repo/releases/download/$version/voiced_${os}_${arch}.tar.gz"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
curl -fL "$url" -o "$tmp/voiced.tar.gz"
tar -xzf "$tmp/voiced.tar.gz" -C "$tmp"
mkdir -p "$bin_dir"
install -m 0755 "$tmp/voiced" "$bin_dir/voiced"
echo "Installed $bin_dir/voiced"
"$bin_dir/voiced" version
