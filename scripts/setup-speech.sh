#!/usr/bin/env sh
set -eu

# Installs the default local speech engines used by herdr:
#   - whisper.cpp for speech-to-text (built natively for this CPU)
#   - Piper for text-to-speech (official prebuilt archive)
# Models live outside the repository under $XDG_DATA_HOME/herdr/models.
# Nothing here is linked into the herdr binary; config may select other engines.

WHISPER_VERSION=${WHISPER_VERSION:-v1.9.4}
PIPER_VERSION=${PIPER_VERSION:-2023.11.14-2}
DATA=${XDG_DATA_HOME:-"$HOME/.local/share"}/herdr
CACHE=${XDG_CACHE_HOME:-"$HOME/.cache"}/herdr
BIN=${HOME}/.local/bin
MODELS=$DATA/models
mkdir -p "$DATA" "$CACHE" "$BIN" "$MODELS"

arch=$(uname -m)
case "$arch" in
  aarch64|arm64) piper_arch=aarch64 ;;
  x86_64|amd64) piper_arch=x86_64 ;;
  *) echo "unsupported Piper architecture: $arch" >&2; exit 1 ;;
esac

if ! command -v whisper-cli >/dev/null 2>&1; then
  src="$CACHE/whisper.cpp-$WHISPER_VERSION"
  rm -rf "$src"
  git clone --depth 1 --branch "$WHISPER_VERSION" https://github.com/ggml-org/whisper.cpp.git "$src"
  cmake -S "$src" -B "$src/build" -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON
  cmake --build "$src/build" --config Release -j "$(nproc)"
  install -m 0755 "$src/build/bin/whisper-cli" "$BIN/whisper-cli"
fi

if [ ! -x "$DATA/piper/piper" ]; then
  archive="$CACHE/piper_linux_${piper_arch}.tar.gz"
  curl -fL "https://github.com/rhasspy/piper/releases/download/$PIPER_VERSION/piper_linux_${piper_arch}.tar.gz" -o "$archive"
  rm -rf "$DATA/piper"
  mkdir -p "$DATA/piper"
  tar -xzf "$archive" -C "$DATA/piper" --strip-components=1
fi
ln -sf "$DATA/piper/piper" "$BIN/piper"

if [ ! -s "$MODELS/ggml-base.en.bin" ]; then
  curl -fL https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin -o "$MODELS/ggml-base.en.bin.tmp"
  mv "$MODELS/ggml-base.en.bin.tmp" "$MODELS/ggml-base.en.bin"
fi

voice_base=https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/amy/medium
for ext in onnx onnx.json; do
  file="en_US-amy-medium.$ext"
  if [ ! -s "$MODELS/$file" ]; then
    curl -fL "$voice_base/$file" -o "$MODELS/$file.tmp"
    mv "$MODELS/$file.tmp" "$MODELS/$file"
  fi
done

echo "Speech engines installed:"
"$BIN/whisper-cli" --help 2>&1 | sed -n '1p'
"$BIN/piper" --help 2>&1 | sed -n '1p'
echo "Models: $MODELS"
