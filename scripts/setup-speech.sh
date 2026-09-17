#!/usr/bin/env sh
set -eu

# Installs the default local speech engines used by Voice:
#   - whisper.cpp for speech-to-text (built natively for this CPU)
#   - Piper for text-to-speech (official prebuilt archive)
# Models live outside the repository under $XDG_DATA_HOME/voice/models.
# Nothing here is linked into the Voice binary; config may select other engines.

WHISPER_VERSION=${WHISPER_VERSION:-v1.9.4}
PIPER_VERSION=${PIPER_VERSION:-2023.11.14-2}
DATA=${XDG_DATA_HOME:-"$HOME/.local/share"}/voice
CACHE=${XDG_CACHE_HOME:-"$HOME/.cache"}/voice
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
  # BUILD_SHARED_LIBS=OFF is load-bearing: whisper.cpp defaults to shared
  # libraries, and installing only the binary leaves it looking for
  # libwhisper.so.1 in loader paths it was never installed into. That shipped
  # a speech engine that could not start, and because the failure happens at
  # exec time the service logged it once per utterance and transcribed
  # nothing. Linking statically means there is one file to install.
  cmake -S "$src" -B "$src/build" -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON
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

# Verify by running each engine and checking its exit status. The previous
# version piped --help into sed, so the pipeline succeeded whatever the engine
# did: an engine that could not load its own libraries reported as installed.
check_engine() {
  name=$1
  path=$2
  if out=$("$path" --help 2>&1); then
    printf '%s: %s\n' "$name" "$(printf '%s' "$out" | sed -n '1p')"
    return 0
  fi
  status=$?
  # 127 and the loader's own message are the signature of a binary that
  # cannot find its shared libraries.
  printf '%s failed to run (exit %s):\n%s\n' "$name" "$status" "$out" >&2
  case "$out" in
    *"error while loading shared libraries"*)
      printf 'The engine was built against shared libraries that are not installed.\n' >&2
      ;;
  esac
  return 1
}

echo "Speech engines installed:"
failed=0
check_engine whisper-cli "$BIN/whisper-cli" || failed=1
check_engine piper "$BIN/piper" || failed=1
if [ "$failed" -ne 0 ]; then
  echo "Speech setup did not produce working engines; Voice would log a failure per utterance and transcribe nothing." >&2
  exit 1
fi
echo "Models: $MODELS"
