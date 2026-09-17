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

# capture SECONDS LABEL [DEVICE] -> prints the peak of one capture, or says
# why it could not take one.
#
# The first version suppressed arecord's stderr and fed whatever appeared into
# wave.open, so an ALSA failure surfaced as a Python EOFError on a truncated
# file and the script carried on as though it had a measurement. A diagnostic
# that cannot tell "the device returned silence" from "the capture never
# happened" is the exact ambiguity this investigation has been fighting, and it
# aborted the owner's session for no reason.
capture() {
	local secs="$1" label="$2" dev="${3:-$DEV}" wav status=0
	wav=$(mktemp /tmp/powerconf-XXXXXX.wav)
	# arecord's stderr is the ALSA diagnostic and the most useful thing this
	# function can produce when something goes wrong, so it is kept.
	setpriv --reuid="$SERVICE_UID" --regid="$SERVICE_GID" --groups="$SERVICE_GROUPS" \
		arecord -D "$dev" -f S16_LE -r 16000 -c 1 -d "$secs" "$wav" || status=$?
	if [ "$status" -ne 0 ]; then
		printf '    %s: CAPTURE FAILED, arecord exit %s on %s (ALSA error above)\n' "$label" "$status" "$dev"
		rm -f "$wav"
		return 1
	fi
	if [ ! -s "$wav" ]; then
		printf '    %s: CAPTURE FAILED, arecord wrote an empty file on %s\n' "$label" "$dev"
		rm -f "$wav"
		return 1
	fi
	python3 - "$wav" "$label" "$dev" <<'PY'
import sys, wave, audioop
path, label, dev = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with wave.open(path) as w:
        frames = w.readframes(w.getnframes())
except Exception as err:
    print(f"    {label}: CAPTURE UNREADABLE on {dev}: {type(err).__name__}: {err}")
    raise SystemExit(1)
if not frames:
    print(f"    {label}: CAPTURE EMPTY on {dev}: no audio frames")
    raise SystemExit(1)
print(f"    {label}: peak={audioop.max(frames, 2)} frames={len(frames)//2} device={dev}")
PY
	status=$?
	rm -f "$wav"
	return "$status"
}

# restore puts the device and the service back however this script exits,
# including a mid-phase failure. Without it, an abort left the speakerphone
# holding whatever report was written last and the service stopped.
restore() {
	if ! hid '\x02\x00\x00' 2>/dev/null; then
		printf 'WARNING: could not clear the telephony report on %s\n' "$HIDRAW" >&2
	fi
	if ! systemctl start voice >/dev/null 2>&1; then
		printf 'WARNING: could not restart voice; run: systemctl start voice\n' >&2
	fi
}
trap restore EXIT

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
echo "EXPECTED IF CAPTURE WORKS: a 'heard=...' line CONTAINING the phrase, with"
echo "a peak well above 0.02. Any other transcript is a FAIL."
echo "EXPECTED IF STILL SILENT: no heard= line at all."

# Attribution. Whether those lines come from the service's own capture loop or
# from a CLI invocation has been the open question behind every conclusion on
# issue #9, and testimony is not the same evidence as metadata. The script
# collects it so the answer cannot depend on anyone remembering to ask.
echo
echo "--- who produced those lines (service MainPID vs anything else)"
systemctl show -p MainPID -p InvocationID voice
# python reads its program from a file, not from stdin: the journal is on
# stdin, and a heredoc would have replaced it - the script would have
# printed nothing and looked like an absence of evidence.
meta=$(mktemp /tmp/powerconf-meta-XXXXXX.py)
cat >"$meta" <<'META'
import json
import sys

for line in sys.stdin:
    try:
        entry = json.loads(line)
    except ValueError:
        continue
    message = entry.get("MESSAGE", "")
    if "transcribe" not in message and "heard=" not in message and "stage=" not in message:
        continue
    print(
        "    pid={pid} comm={comm} ident={ident} :: {msg}".format(
            pid=entry.get("_PID"),
            comm=entry.get("_COMM"),
            ident=entry.get("SYSLOG_IDENTIFIER"),
            msg=message[:90],
        )
    )
META
journalctl -u voice -o json --since "-3 minutes" | python3 "$meta"
rm -f "$meta"
echo "READ THIS: if those pids equal the MainPID above, the service's own"
echo "capture loop heard him and issue #9 is answered by observation. If they"
echo "do not, the lines came from something else and the microphone question is"
echo "still open - which is the answer the coordinator has been asking for."

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
