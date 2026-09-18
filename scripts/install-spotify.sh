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
pebble_card=V3

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
proof=$(mktemp -d)
trap 'rm -rf "$proof"' EXIT
cat > "$proof/broken.conf" <<CONF
</usr/share/alsa/alsa.conf>
pcm.!default {
    type hw
    card "NoSuchCardExists"
}
CONF
if ! command -v aplay > /dev/null; then
  echo "aplay is required (package alsa-utils) to prove the ALSA namespace is load-bearing" >&2
  exit 1
fi
if broken_out=$(ALSA_CONFIG_PATH="$proof/broken.conf" aplay -d 1 /dev/zero 2>&1); then
  echo "ALSA_CONFIG_PATH is being IGNORED: playback succeeded against a config naming a nonexistent card." >&2
  echo "The Pebble pin would be decorative and the client could open any device, so refusing to continue." >&2
  exit 1
fi
# The failure has to be ATTRIBUTABLE to the broken card name. A missing
# binary, a busy device or a permissions error also make the command fail,
# and accepting any nonzero status here would be a check that passes for
# reasons unrelated to what it claims to prove - which is the defect this
# whole check exists to avoid.
case "$broken_out" in
  *NoSuchCardExists* | *"no such"* | *"No such"* | *"cannot find card"* | *"Unknown PCM"* | *"unknown card"*) ;;
  *)
    echo "playback failed, but not because of the nonexistent card:" >&2
    echo "$broken_out" >&2
    echo "so this does not establish that ALSA_CONFIG_PATH is honoured; refusing to continue." >&2
    exit 1
    ;;
esac

# And the real one must work, or the pin is pointing at nothing.
if ! ALSA_CONFIG_PATH="$alsa_conf" aplay -l 2>/dev/null | grep -q "$pebble_card"; then
  echo "the Pebble (card $pebble_card) is not present; plug it in before installing" >&2
  exit 1
fi
echo "ALSA namespace verified load-bearing: a nonexistent card fails, the Pebble resolves."

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
