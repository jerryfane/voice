#!/usr/bin/env sh
# Install the Spotify client for Voice, pinned to the Creative Pebble V3 and
# unable to reach the microphone.
#
# Everything this script installs lives in the repository. A rebuild or a
# reimage reproduces it; nothing here depends on someone remembering what was
# typed on the device once.
#
# It stops before the Spotify login. That step needs the owner.
set -eu

alsa_conf=/etc/voice/spotify-alsa.conf
account=spotify-player
state=/var/lib/spotify-player

cd "$(dirname "$0")/.."

if ! command -v setfacl > /dev/null; then
  echo "setfacl is required (package acl): the udev rule uses it to grant the client access to the Pebble only" >&2
  exit 1
fi

if ! id -u "$account" > /dev/null 2>&1; then
  # Deliberately NOT in the audio group. Membership there would grant every
  # card, including the capture device the Voice service holds; the udev rule
  # grants the Pebble alone.
  sudo useradd --system --user-group --home-dir "$state" --shell /usr/sbin/nologin "$account"
fi
sudo mkdir -p "$state/config" "$state/cache"
sudo chown -R "$account:$account" "$state"

sudo install -m 0644 packaging/spotify/alsa.conf "$alsa_conf"
sudo install -m 0644 packaging/spotify/99-voice-spotify-pebble.rules \
  /etc/udev/rules.d/99-voice-spotify-pebble.rules
sudo install -m 0755 packaging/spotify/spotify-player-run /usr/local/bin/spotify-player-run
sudo install -m 0644 packaging/spotify/spotify-player.service /etc/systemd/system/spotify-player.service
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=sound

# The client cannot name an output device, so "default" is defined for it by
# the ALSA namespace above. That only helps if the namespace is actually being
# read - and ALSA_CONFIG_PATH being ignored would look exactly like it working,
# since audio would still play, just out of the wrong device.
#
# So: prove the file is load-bearing before trusting it. Point its default at a
# card that does not exist and require playback to FAIL.
# The client cannot name an output device, so "default" is defined for it by
# the ALSA namespace above. That only helps if the namespace is actually being
# read - and ALSA_CONFIG_PATH being ignored would look exactly like it working,
# since audio would still play, just out of the wrong device.
#
# So prove the namespace is load-bearing with a PAIRED CONTROL: the same
# binary, the same invocation, twice.
#
#   bogus namespace -> playback MUST fail
#   real  namespace -> the identical command MUST succeed
#
# The pairing is what distinguishes "the namespace decides the device" from
# "aplay is broken, absent, or the file is unreadable": a broken aplay fails
# BOTH arms, and an ignored config passes both. An earlier version accepted any
# nonzero status from the negative arm alone, which passed on a host where
# aplay did not exist - "playback failed" meaning "command not found". A check
# that could not fail, inside the check meant to prevent one.
if ! command -v aplay > /dev/null; then
  echo "aplay is required (package alsa-utils) to prove the ALSA namespace is load-bearing" >&2
  exit 1
fi

proof=$(mktemp -d)
trap 'rm -rf "$proof"' EXIT
cat > "$proof/bogus.conf" <<CONF
</usr/share/alsa/alsa.conf>
pcm.!default {
    type hw
    card "NoSuchCardExists"
}
CONF

probe_playback() {
  ALSA_CONFIG_PATH="$1" aplay -q -d 1 -f cd /dev/zero > "$proof/out" 2>&1
}

if probe_playback "$proof/bogus.conf"; then
  echo "ALSA_CONFIG_PATH is being IGNORED: playback succeeded against a config naming a nonexistent card." >&2
  echo "The Pebble pin would be decorative, so refusing to continue." >&2
  exit 1
fi
negative_output=$(cat "$proof/out")

if ! probe_playback "$alsa_conf"; then
  # Both arms failed, so this proves nothing about the namespace: aplay may be
  # broken, the Pebble may be absent, or the device may be busy. Report it as
  # UNPROVEN rather than concluding from the negative arm alone.
  echo "UNPROVEN: playback failed with the real namespace too, so the negative above shows nothing." >&2
  echo "  with the bogus namespace: $negative_output" >&2
  echo "  with $alsa_conf: $(cat "$proof/out")" >&2
  echo "Fix playback to the Pebble first - it must be plugged in and free - then re-run." >&2
  exit 1
fi

echo "ALSA namespace verified load-bearing: a nonexistent card fails, the Pebble plays."

if ! command -v cargo > /dev/null; then
  echo "cargo is required to build spotify_player" >&2
  exit 1
fi
if ! command -v spotify_player > /dev/null; then
  # ALSA backend only. No PulseAudio or PipeWire: the Voice service owns the
  # audio stack and the desktop session is not where this belongs.
  cargo install spotify_player --locked --no-default-features --features alsa-backend,daemon
  sudo install -m 0755 "$HOME/.cargo/bin/spotify_player" /usr/local/bin/spotify_player
fi

sudo systemctl daemon-reload
echo
echo "Installed. The client is pinned to the Pebble by name and cannot open the microphone."
echo
echo "NOT done, and needs the owner: Spotify login. Run"
echo "  sudo -u $account -H env ALSA_CONFIG_PATH=$alsa_conf XDG_CONFIG_HOME=$state/config /usr/local/bin/spotify_player authenticate"
echo "and follow the prompt. Then: sudo systemctl enable --now spotify-player"
