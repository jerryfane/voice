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
failures=0
# A capture that never ran is not a capture that recorded silence. Every
# failure increments this, the run ends with an explicit verdict, and the exit
# status is nonzero - so a phase that produced no measurement cannot be quoted
# as evidence of a quiet microphone.
note_failure() {
	failures=$((failures + 1))
	printf '    ^ THAT STEP PRODUCED NO MEASUREMENT: %s\n' "$1"
	printf '      Nothing below may be read as "the device returned silence".\n'
}

DEV=${DEV:-plughw:2,0}
OUTDEV=${OUTDEV:-plughw:2,0}
SERVICE_UID=${SERVICE_UID:-995}
SERVICE_GID=${SERVICE_GID:-989}
SERVICE_GROUPS=${SERVICE_GROUPS:-29,44,989}
LOG=/tmp/powerconf-wake-attempt.log
DWELL=${DWELL:-8}

hid() { printf "$1" >"$HIDRAW"; }

# DRY_RUN=1 executes every automated phase with the human steps stubbed, so
# the instrument can be proven on the real hardware BEFORE a person is asked to
# stand at the speaker. Two sessions have now died on defects in this script,
# and the owner's time is the scarcest resource in the program: nothing that
# needs him runs until a dry run has produced a measurement or a named failure
# at every step. It is deliberately not a simulation - mixer reads, both-device
# captures, the HID walk, restore and service restart all really run.
DRY_RUN=${DRY_RUN:-0}

ask() {
	if [ "$DRY_RUN" = "1" ]; then
		printf '\n>>> [DRY RUN, nobody asked] %s\n' "$1"
		return 0
	fi
	printf '\n>>> %s\n>>> press Enter to continue: ' "$1"
	read -r _
}

# Steps whose whole purpose is a human observation - reading the ring colour,
# pressing the mute button, speaking the phrase - cannot be stubbed into a
# result. A dry run says so out loud rather than printing something that could
# later be quoted as an observation nobody made.
human_only() {
	if [ "$DRY_RUN" = "1" ]; then
		printf '    [DRY RUN] NOT MEASURED, needs a person: %s\n' "$1"
		return 1
	fi
	return 0
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
	# mktemp creates the file as ROOT, but arecord runs as the service uid, so
	# without this it cannot write and reports Permission denied - which this
	# function would then have printed as CAPTURE FAILED, an unmeasured run
	# looking like a device problem. Caught on the device by the pi-burj seat.
	if ! chown "$SERVICE_UID:$SERVICE_GID" "$wav"; then
		printf '    %s: SETUP FAILED, cannot chown %s to %s:%s\n' \
			"$label" "$wav" "$SERVICE_UID" "$SERVICE_GID"
		rm -f "$wav"
		return 1
	fi
	# And prove the service user can actually write it before blaming the
	# microphone for whatever comes next.
	if ! setpriv --reuid="$SERVICE_UID" --regid="$SERVICE_GID" \
		--groups="$SERVICE_GROUPS" test -w "$wav"; then
		printf '    %s: SETUP FAILED, uid %s cannot write %s\n' \
			"$label" "$SERVICE_UID" "$wav"
		rm -f "$wav"
		return 1
	fi
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
import array
import sys
import wave
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
samples = array.array("h")
samples.frombytes(frames)
peak = max(abs(s) for s in samples)
import os

print(
    f"    {label}: peak={peak} frames={len(samples)} "
    f"bytes={os.path.getsize(path)} device={dev}"
)
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

human_only "the ring colour (red usually means muted)" \
	&& ask "LOOK at the ring now and write down its colour (red usually means muted)"
capture 3 "capture as found" || note_failure "capture as found"

echo "Now reading HID input reports for 15 s. Press the PowerConf's mute button"
echo "ONCE during that window; a report appearing proves the button reaches the host."
timeout 15 cat "$HIDRAW" | od -An -tx1 -v | sed -n '1,10p' &
reader=$!
if human_only "a mute button press"; then
	ask "press the mute button once now, then press Enter"
else
	echo "    [DRY RUN] reading hidraw unattended for the full window; a report"
	echo "    appearing without a press would itself be worth knowing."
	sleep 15
fi
wait "$reader" 2>/dev/null
capture 3 "capture after toggling mute" || note_failure "capture after toggling mute"

echo "If the second capture is still zero, toggle the button once more and repeat:"
human_only "a second mute button press" \
	&& ask "press the mute button once more (back to the other state), then press Enter"
capture 3 "capture after second toggle" || note_failure "capture after second toggle"

echo
echo "=== phase 1b: does the RUNNING SERVICE contend for the capture device"
echo "Every capture in phase 1 was taken with voice stopped. If the running"
echo "service holds the device exclusively, then any capture taken while it ran"
echo "was competing with it - and any conclusion drawn from such a capture is"
echo "void, whatever number it produced. This measures both states so the"
echo "difference is on the record instead of assumed."
echo "Noted against myself: this check existed only in my instructions to the"
echo "device seat for three rounds, which is why it kept not happening. It is"
echo "in the script now."
if ! systemctl start voice >/dev/null 2>&1; then
	note_failure "could not start voice for the contention test"
else
	sleep 3
	printf '    service state: %s\n' "$(systemctl is-active voice)"
	capture 3 "capture with the service ACTIVE" || note_failure "capture with the service ACTIVE"
fi
systemctl stop voice >/dev/null 2>&1
sleep 1
printf '    service state: %s\n' "$(systemctl is-active voice)"
capture 3 "capture with the service STOPPED" || note_failure "capture with the service STOPPED"
echo "READ THIS: if ACTIVE fails or reads differently from STOPPED, the service"
echo "and this script cannot both hold the microphone, and every earlier"
echo "reading taken with voice running is void for that reason alone. If both"
echo "read the same, contention is ruled out and that is worth knowing too."

echo "=== phase 1c: does the microphone hear a KNOWN SOUND (no human needed)"
echo "Every capture so far was taken in a quiet room with nobody speaking, so"
echo "peak=0 was the EXPECTED result and proves nothing about the microphone."
echo "This plays a 1 kHz tone out of the PowerConf's own speaker while its own"
echo "microphone records, which separates the two explanations without anyone"
echo "having to be present."
acoustic_probe() {
	tone=$(mktemp /tmp/powerconf-tone-XXXXXX.wav)
	python3 - "$tone" <<'TONE'
import array
import math
import sys
import wave

rate = 48000
seconds = 4
# Half amplitude: loud enough to be unmistakable, not clipping.
samples = array.array(
    "h",
    (int(16000 * math.sin(2 * math.pi * 1000 * n / rate)) for n in range(rate * seconds)),
)
with wave.open(sys.argv[1], "wb") as w:
    w.setnchannels(1)
    w.setsampwidth(2)
    w.setframerate(rate)
    w.writeframes(samples.tobytes())
TONE
	if [ ! -s "$tone" ]; then
		printf '    SETUP FAILED, could not generate the tone file\n'
		rm -f "$tone"
		return 1
	fi
	# Whether the speaker actually emitted the tone decided nothing last run,
	# because the only witness would have been a person in the room. ALSA can
	# answer it without one: the playback substream's state and hw_ptr say
	# whether frames really moved to the device. A peak of zero means nothing
	# unless playback is proven to have run.
	card=$(printf '%s' "$OUTDEV" | sed -n 's/.*hw:\([0-9][0-9]*\).*/\1/p')
	[ -n "$card" ] || card=0
	printf '    playback mixer state on card %s:\n' "$card"
	amixer -c "$card" sget 'PCM' 2>/dev/null | sed 's/^/      /' \
		|| printf '      (no PCM playback control; see the phase 0 mixer dump)\n'
	aplay -D "$OUTDEV" "$tone" >/dev/null 2>&1 &
	player=$!
	# Sample both substreams mid-tone, so the capture reading arrives together
	# with evidence about whether audio moved in either direction.
	(
		sleep 2
		printf '    playback substream during the tone:\n'
		sed -n '1,4p' "/proc/asound/card$card/pcm0p/sub0/status" 2>/dev/null | sed 's/^/      /' \
			|| printf '      (no playback substream status available)\n'
		printf '    capture substream during the tone:\n'
		sed -n '1,4p' "/proc/asound/card$card/pcm0c/sub0/status" 2>/dev/null | sed 's/^/      /' \
			|| printf '      (no capture substream status available)\n'
	) &
	sampler=$!
	# Let the tone actually start before recording, or the capture measures
	# the silence before playback and repeats the original mistake.
	sleep 1
	capture 3 "capture WHILE a 1 kHz tone plays from the device speaker"
	status=$?
	wait "$player" 2>/dev/null
	wait "$sampler" 2>/dev/null
	rm -f "$tone"
	return "$status"
}
acoustic_probe || note_failure "acoustic self-test (tone through the device speaker)"
echo "HOW TO READ THE SUBSTREAM LINES: state RUNNING with hw_ptr advancing on"
echo "the PLAYBACK substream proves frames reached the device, so a capture peak"
echo "of zero is then a real microphone-path result rather than an untested"
echo "assumption that something was playing. If playback is not RUNNING, the"
echo "tone never left the host and the microphone question stays OPEN - which is"
echo "the state the previous run was actually in, though it could not tell."
echo "READ THIS: a peak well above the quiet-room readings means the microphone"
echo "path works and the earlier zeros were an empty room, not a broken device."
echo "A peak that stays at zero WHILE the tone is audible means the capture path"
echo "is genuinely not carrying audio, and that conclusion no longer depends on"
echo "anyone speaking. If the tone is inaudible, playback is the broken half and"
echo "the microphone question is still open - say which you heard."

echo
echo "=== phase 1d: capture with OFF-HOOK HELD - the only arm that can answer #9"
echo "The coordinator found the flaw in the two arms above, and it is decisive:"
echo "with the service ACTIVE it holds the capture device, so there is no"
echo "measurement; with the service STOPPED nothing holds the telephony"
echo "off-hook bit, and this repo's own issue #8 work established that capture"
echo "holds off-hook for the whole session because otherwise the PowerConf"
echo "streams digital silence. So 'service stopped' is exactly the condition in"
echo "which a WORKING microphone is expected to return zeros. Both arms were"
echo "uninformative by construction, and peak 0 from either says nothing about"
echo "the hardware."
echo "This arm removes both confounds: the service stays stopped so nothing"
echo "contends, and the probe itself holds the off-hook report open on the"
echo "hidraw node for the whole capture."
offhook_capture() {
	# The descriptor is held OPEN across the capture. Writing the report and
	# closing - which is what hid() does - is not the same thing: the device
	# can drop back to idle the moment the owner of the node goes away, which
	# is the difference between this arm and the held-descriptor runs that
	# predate the frame-count fix and are void.
	if ! exec 9>"$HIDRAW"; then
		printf '    SETUP FAILED, cannot open %s for the held-open report\n' "$HIDRAW"
		return 1
	fi
	if ! printf '\x02\x01\x00' >&9; then
		printf '    SETUP FAILED, cannot write the off-hook report to %s\n' "$HIDRAW"
		exec 9>&-
		return 1
	fi
	printf '    off-hook report written and descriptor HELD OPEN (fd 9)\n'
	sleep 1
	capture 3 "capture with off-hook HELD (loopback control follows)"
	status=$?
	# Prove the descriptor was still open at the end rather than assuming it.
	if printf '\x02\x01\x00' >&9 2>/dev/null; then
		printf '    off-hook descriptor still writable AFTER the capture: held\n'
	else
		printf '    WARNING: off-hook descriptor was NOT writable after the capture,\n'
		printf '    so this arm did not hold what it claims and proves nothing\n'
		status=1
	fi
	exec 9>&-
	return "$status"
}
offhook_capture || note_failure "capture with off-hook held"
# The loopback control answers the one question this arm cannot: whether the
# reading code can report a nonzero peak at all on this host, right now.
if [ -n "${LOOPDEV:-}" ]; then
	capture 3 "loopback control AFTER the off-hook arm" "$LOOPDEV" \
		|| note_failure "loopback control after the off-hook arm"
else
	echo "    (set LOOPDEV to run the loopback control beside this arm)"
fi
echo "HOW TO READ THIS ARM, and it is the only arm whose result is evidence:"
echo "nonzero peak means capture WORKS and every historical zero is explained"
echo "by method rather than hardware. Real frames at peak zero, WITH the"
echo "off-hook descriptor proven held either side and the loopback control"
echo "nonzero, is the first genuine evidence of a hardware or firmware mute -"
echo "and only then is the owner's mute-button press worth his time."

echo
echo "=== phase 2: live wake attempt"
hid '\x02\x00\x00'
systemctl start voice
sleep 3
: >"$LOG"
journalctl -u voice -f -n 0 >"$LOG" 2>&1 &
follower=$!
sleep 2
if human_only "the spoken phrase 'Hey Voice, what time is it'"; then
	ask "say clearly, 30 cm from the device: 'Hey Voice, what time is it' - then wait 3 s and press Enter"
	sleep 3
else
	# Still exercise the journal capture and the attribution block, so a dry
	# run proves the plumbing that would record his utterance actually works.
	echo "    [DRY RUN] journal armed and attribution block exercised on whatever"
	echo "    the service logs unprompted; no transcript is claimed."
	sleep 5
fi
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
	# The HID write and the dwell really happen in a dry run - that is the
	# part that can break - but the observation is a person's, so it is
	# reported as NOT MEASURED rather than as a prompt nobody answered.
	human_only "the ring appearance for: $2" \
		&& ask "write down what the ring looked like for: $2"
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

# Final verdict. A run that failed to measure exits nonzero and says so, so the
# absence of a number is never mistaken for the number zero.
echo
if [ "$DRY_RUN" = "1" ]; then
	echo "=== DRY RUN. The human-only steps above are NOT MEASURED and must not"
	echo "be quoted as observations. What a clean dry run proves is narrower and"
	echo "is exactly the gate that matters: every automated step ran on the real"
	echo "hardware and produced either a measurement or a named failure, so the"
	echo "instrument is fit to put in front of a person."
fi
if [ "$failures" -eq 0 ]; then
	echo "=== every capture step produced a measurement (peak and frame count)."
else
	printf '=== %s capture step(s) PRODUCED NO MEASUREMENT.\n' "$failures"
	echo "Those steps say nothing about what the microphone heard. Do not quote"
	echo "them as silence; fix the reported cause and re-run."
	exit 1
fi
