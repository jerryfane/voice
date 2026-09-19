#!/usr/bin/env sh
set -eu

# Installs the default local speech engines used by Voice:
#   - Sherpa-ONNX's 3M streaming keyword model
#   - whisper.cpp CLI and resident HTTP server (built natively for this CPU)
#   - Piper for text-to-speech (official prebuilt archive)
# Models live outside the repository under $XDG_DATA_HOME/voice/models.

WHISPER_VERSION=${WHISPER_VERSION:-v1.9.4}
PIPER_VERSION=${PIPER_VERSION:-2023.11.14-2}
KWS_MODEL=${KWS_MODEL:-sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20}
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

if [ ! -x "$BIN/whisper-cli" ] || [ ! -x "$BIN/whisper-server" ]; then
  src="$CACHE/whisper.cpp-$WHISPER_VERSION"
  rm -rf "$src"
  git clone --depth 1 --branch "$WHISPER_VERSION" https://github.com/ggml-org/whisper.cpp.git "$src"
  # BUILD_SHARED_LIBS=OFF is load-bearing: whisper.cpp defaults to shared
  # libraries, and installing only the binary leaves it looking for
  # libwhisper.so.1 in loader paths it was never installed into. That shipped
  # a speech engine that could not start, and because the failure happens at
  # exec time the service logged it once per utterance and transcribed
  # nothing. With it, whisper.cpp's own libraries are linked into the
  # executable, so there is one file to install and no libwhisper.so.1 to
  # find.
  #
  # Stated precisely, because the loose version of this claim is how the
  # original defect passed review: the result is NOT a static binary. On the
  # installed arm64 host, ldd whisper-cli still resolves libgomp, libstdc++,
  # libm, libgcc_s and libc from the distribution. Those ship with the base
  # system and are why the reported failure cannot recur; whisper.cpp's own
  # shared objects were the ones nobody installed.
  cmake -S "$src" -B "$src/build" -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON
  cmake --build "$src/build" --config Release -j "$(nproc)"
  install -m 0755 "$src/build/bin/whisper-cli" "$BIN/whisper-cli"
  install -m 0755 "$src/build/bin/whisper-server" "$BIN/whisper-server"
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

kws_dir="$MODELS/sherpa-kws"
if [ ! -s "$kws_dir/encoder.int8.onnx" ] ||
   [ ! -s "$kws_dir/decoder.onnx" ] ||
   [ ! -s "$kws_dir/joiner.int8.onnx" ] ||
   [ ! -s "$kws_dir/tokens.txt" ]; then
  archive="$CACHE/$KWS_MODEL.tar.bz2"
  extracted="$CACHE/$KWS_MODEL"
  curl -fL "https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/$KWS_MODEL.tar.bz2" -o "$archive.tmp"
  mv "$archive.tmp" "$archive"
  rm -rf "$extracted"
  tar -xjf "$archive" -C "$CACHE"
  mkdir -p "$kws_dir"
  install -m 0644 "$extracted/encoder-epoch-13-avg-2-chunk-8-left-64.int8.onnx" "$kws_dir/encoder.int8.onnx"
  install -m 0644 "$extracted/decoder-epoch-13-avg-2-chunk-8-left-64.onnx" "$kws_dir/decoder.onnx"
  install -m 0644 "$extracted/joiner-epoch-13-avg-2-chunk-8-left-64.int8.onnx" "$kws_dir/joiner.int8.onnx"
  install -m 0644 "$extracted/tokens.txt" "$kws_dir/tokens.txt"
fi
printf '%s\n' \
  'HH EY1 V OY1 S :4.0 #0.05 @HEY_VOICE' \
  'HH EY1 B OY1 Z :4.0 #0.05 @HEY_BOYS' \
  > "$kws_dir/keywords.txt"

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
  # Capture the status explicitly. `if out=$(...); then ... fi` followed by
  # $? reports the status of the if-compound, which is 0 when neither branch
  # body ran - so the previous version printed "(exit 0)" for every failure,
  # including a 127. An unfailable check is what this function exists to
  # replace; getting its own diagnostics wrong was the same mistake one layer
  # in.
  status=0
  out=$("$path" --help 2>&1) || status=$?

  # Only a loader failure is fatal. Engines disagree about the exit status of
  # --help, so a non-zero exit with real output is not evidence of anything -
  # the same rule the Go-side probe applies, and it has to match or the two
  # checks disagree about what a working engine looks like.
  case "$out" in
    *"error while loading shared libraries"*)
      printf '%s cannot start: %s\n' "$name" "$(printf '%s' "$out" | sed -n '1p')" >&2
      printf 'It was built against shared libraries that are not installed.\n' >&2
      return 1
      ;;
  esac
  if [ "$status" -eq 127 ]; then
    printf '%s cannot start (exit 127):\n%s\n' "$name" "$out" >&2
    return 1
  fi
  if [ -z "$out" ] && [ "$status" -ne 0 ]; then
    printf '%s failed to run (exit %s) with no output\n' "$name" "$status" >&2
    return 1
  fi
  printf '%s: %s\n' "$name" "$(printf '%s' "$out" | sed -n '1p')"
  return 0
}

echo "Speech engines installed:"
failed=0
check_engine whisper-cli "$BIN/whisper-cli" || failed=1
check_engine whisper-server "$BIN/whisper-server" || failed=1
check_engine piper "$BIN/piper" || failed=1
if [ "$failed" -ne 0 ]; then
  echo "Speech setup did not produce working engines; Voice would log a failure per utterance and transcribe nothing." >&2
  exit 1
fi
echo "Models: $MODELS"
