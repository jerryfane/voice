#!/usr/bin/env bash
# Single-pass physical check for the Anker PowerConf on this Pi.
# Staged for jerryfane/voice issue #9 (capture produces digital silence) and
# issue #2 (which LED reads as "listening"). Needs a human in the room.
#
# What is already known, so this session does not repeat it:
#   - the USB capture endpoint streams (stream0: Status Running, 48 kHz mono)
#     and delivers exact zeros;
#   - ALSA 'Mic Capture Switch' is on, and plug/raw/48 kHz variants are all
#     equally zero;
#   - an ALSA loopback capture under the service's own uid/gid/groups reads
#     peak 16384 with the same measuring code, so the measurement is sound;
#   - holding the hidraw descriptor open across the capture changes nothing,
#     which falsified the close-after-write hypothesis.
# What is left needs hands: the device's own mute state, and the LED mapping.
#
# Run as: sudo /tmp/powerconf-physical-check.sh
set -u

HIDRAW=${HIDRAW:-/dev/hidraw5}
CARD=${CARD:-2}
DEV=${DEV:-plughw:2,0}
SERVICE_UID=${SERVICE_UID:-995}
SERVICE_GID=${SERVICE_GID:-989}
SERVICE_GROUPS=${SERVICE_GROUPS:-29,44,989}
LOG=/tmp/powerconf-wake-attempt.log
DWELL=${DWELL:-8}

hid() { printf "$1" >"$HIDRAW"; }

ask() {
	printf '\n>>> %s\n>>> press Enter to continue: ' "$1"
	read -r _
}

# capture RATE SECONDS LABEL -> prints the peak of one capture
capture() {
	local secs="$1" label="$2" wav
	wav=$(mktemp /tmp/powerconf-XXXXXX.wav)
	setpriv --reuid="$SERVICE_UID" --regid="$SERVICE_GID" --groups="$SERVICE_GROUPS" \
		arecord -D "$DEV" -f S16_LE -r 16000 -c 1 -d "$secs" "$wav" 2>/dev/null
	python3 - "$wav" "$label" <<'PY'
import sys, wave, audioop
path, label = sys.argv[1], sys.argv[2]
with wave.open(path) as w:
    frames = w.readframes(w.getnframes())
print(f"    {label}: peak={audioop.max(frames, 2)} frames={len(frames)//2}")
PY
	rm -f "$wav"
}

echo "=== phase 0: state"
systemctl is-active voice || true
amixer -c "$CARD" cget numid=5 numid=6 2>&1 | sed -n '1,12p'

echo
echo "=== phase 1: does the device's own mute explain the silence?"
echo "The PowerConf has a hardware mute button. If it is engaged, the device"
echo "streams silence and no host-side setting can undo it; the ring is"
echo "usually red when muted."
systemctl stop voice
sleep 1
hid '\x02\x01\x00' # off-hook, as Voice holds it while running

ask "LOOK at the ring now and write down its colour (red usually means muted)"
capture 3 "capture as found"

echo "Now reading HID input reports for 15 s. Press the PowerConf's mute button"
echo "ONCE during that window; a report appearing proves the button reaches the host."
timeout 15 cat "$HIDRAW" | od -An -tx1 -v | sed -n '1,10p' &
reader=$!
ask "press the mute button once now, then press Enter"
wait "$reader" 2>/dev/null
capture 3 "capture after toggling mute"

echo "If the second capture is still zero, toggle the button once more and repeat:"
ask "press the mute button once more (back to the other state), then press Enter"
capture 3 "capture after second toggle"

echo
echo "=== phase 2: live wake attempt"
hid '\x02\x00\x00'
systemctl start voice
sleep 3
: >"$LOG"
journalctl -u voice -f -n 0 >"$LOG" 2>&1 &
follower=$!
sleep 2
ask "say clearly, 30 cm from the device: 'Hey Voice, what time is it' - then wait 3 s and press Enter"
sleep 3
kill "$follower" 2>/dev/null
wait "$follower" 2>/dev/null
echo "--- journal during the attempt"
cat "$LOG"
echo "--- utterances seen"
grep -c 'heard=' "$LOG" || true
echo "EXPECTED IF CAPTURE WORKS: a 'heard=...' line with a nonzero peak."
echo "EXPECTED IF STILL SILENT: no heard= line at all."

echo
echo "=== phase 3: which LED reads as listening (issue #2)"
echo "Voice holds off-hook (report 2 bit 0) for its whole session, so the"
echo "question is which EXTRA bit changes the ring versus off-hook alone."
systemctl stop voice
sleep 1

step() {
	hid "$1"
	printf '\n[report 2 = %s] %s\n    expected observable: %s\n    look now (%ss)...\n' "$1" "$2" "$3" "$DWELL"
	sleep "$DWELL"
	ask "write down what the ring looked like for: $2"
}

step '\x02\x00\x00' 'all indicators clear' 'idle appearance'
step '\x02\x01\x00' 'off-hook only (how Voice always runs)' 'in-call appearance - this is the BASELINE'
step '\x02\x11\x00' 'off-hook + microphone LED 0x21' 'a change vs baseline, or none if unimplemented'
step '\x02\x05\x00' 'off-hook + ring LED 0x18' 'a change vs baseline, or none if unimplemented'
step '\x02\x03\x00' 'off-hook + mute LED 0x09' 'likely the muted appearance'
step '\x02\x09\x00' 'off-hook + hold LED 0x20' 'a change vs baseline, or none if unimplemented'

hid '\x02\x00\x00'
systemctl start voice
sleep 2
echo
echo "=== phase 4: restored"
systemctl is-active voice
journalctl -u voice -n 5 --no-pager

cat <<'REPORT'

=== what to report back

1. Ring colour as found, and whether any capture became nonzero after a mute
   toggle. Did pressing the button produce a HID input report?
2. Phase 2: was there a heard= line, and what peak? (no line is a valid answer)
3. Phase 3, per step: did the ring change versus the off-hook baseline, and
   what did it look like? Name the step a person would read as "listening".

Mapping for the winning step, straight into the Voice config:
  microphone LED 0x21 -> feedback.light "mic"    ring LED 0x18 -> "ring"
  mute LED 0x09       -> "mute"                  hold LED 0x20 -> "hold"
  nothing distinguishable -> "none": Voice keeps the acknowledgement sound only.
REPORT
