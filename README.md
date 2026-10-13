# Voice

A persistent, tool-capable voice agent for Linux. Say “Hey Voice,” speak a request, and continue one long-lived OMP conversation with its own workspace and memory.

```text
microphone → Sherpa KWS → command capture → OpenRouter STT → rules → Jev → Herdr OMP
               │                                  │          │
               └ discard ambient                  └ local STT└ bounded actions
                                                     fallback
```

## Agent model

Nontrivial requests run in one persistent OMP session. The session:

- works in `/home/voice-agent/voice-workspace`;
- retains conversation history across voice requests;
- keeps durable facts in `MEMORY.md`;
- stays alive as the named `voice` agent in the normal Herdr session;
- accepts prompts through `herdr agent prompt voice` and publishes final answers
  through a structured OMP extension rather than terminal scraping;
- can list, inspect, and prompt other Herdr agents through a restricted bridge;
- follows the dedicated account’s OMP default model;
- can use shell, read/write, browser, and web-search tools;
- runs as the restricted `voice-agent` OS account, not as your login account;
- cannot read the `pi` account’s private files or authenticated browser profile.

Audio is wake-gated locally by a resident 3M-parameter Sherpa-ONNX keyword spotter. Normally only a bounded utterance accepted after `HEY_VOICE` or `HEY_BOYS` reaches transcription; ambient audio is discarded. An explicitly configured, time- and turn-limited in-flight window can admit follow-up speech without another wake phrase while a wake-authorized agent task is still running. Provider failure falls back to the loopback resident Whisper server and then `whisper-cli`.

## Install

Install speech dependencies and the restricted system service from source:

```sh
./scripts/setup-speech.sh
./scripts/install-local.sh
```

The installer creates `voice-agent`, installs the system services, copies OMP into the restricted runtime, configures its credential-broker connection, provisions the persistent `voice` agent in Herdr, and starts Voice.

The installer creates `/etc/voice/voice.env` as `root:voice-agent` mode `0640`.
To enable OpenRouter transcription and Jev routing, place the key there:

```sh
sudoedit /etc/voice/voice.env
# OPENROUTER_API_KEY=...
sudo systemctl restart voice.service
```

Release binary only:

```sh
curl -fsSL https://raw.githubusercontent.com/jerryfane/voice/main/install.sh | sh
voice init
voice doctor
```

The release installer only installs the `voice` binary. A persistent restricted agent deployment requires `scripts/install-local.sh` from the repository.

## Runtime layout

| Purpose | Path |
|---|---|
| Executable | `/usr/local/bin/voice` |
| System service | `voice.service` |
| Live configuration | `/etc/voice/config.json` |
| Agent account | `voice-agent` |
| Agent workspace | `/home/voice-agent/voice-workspace` |
| Persistent OMP sessions | `/home/voice-agent/.local/share/voice/sessions` |
| Speech models | `/var/lib/voice/models` |
| Credential broker | `voice-auth-broker@<owner>.service` |
| Herdr agent | `voice` in the `voice` workspace |
| Structured response channel | `/home/voice-agent/.local/share/voice/responses` |
| Proposal broker | `voice-proposals-broker.timer` as the owner |
| Proposal store | `/var/lib/voice/proposals.db` |

User-mode configuration defaults to `~/.config/voice/config.json`; override it with `VOICE_CONFIG` or `--config PATH`.

## Use

```sh
voice on
voice off
voice status
voice doctor
voice devices
voice ask 'remember that I prefer concise answers'
voice ask 'what preference did I tell you?'
voice ask 'research the latest Raspberry Pi release in the browser'
voice light bedroom white 60
voice tv tv off
voice say 'Voice is ready.'
```

The long-running listener is managed by systemd:

```sh
sudo systemctl status voice
sudo systemctl restart voice
sudo journalctl -u voice -f
```

The persistent agent is also visible and directly promptable in Herdr:

```sh
herdr agent get voice
herdr agent prompt voice "Summarize your current task." --wait
```

`voice-herdr-ensure` reconciles the stable agent name and starts or resumes its
restricted OMP session if the pane has disappeared.

## Audio and wake phrase

The default wake phrase is exactly:

```text
Hey Voice
```

Voice runs a small streaming keyword model over microphone frames and starts a
bounded command capture only after a configured wake phrase is detected. It
does not run Whisper over ambient segments. With OpenRouter configured, the
accepted command is transcribed by the hosted primary; resident Whisper and
`whisper-cli` remain ordered local fallbacks.

Both interaction styles work:

- Say `Hey Voice`, wait for the acknowledgement, then speak one command. Voice
  waits up to `wake.follow_up_timeout` (6 seconds by default), accepts one
  utterance, and rearms. A timeout stays local and does not call the agent.
- Say `Hey Voice, what's the time?` continuously. Voice retains the original
  inline command behavior instead of playing over the command.

On a Raspberry Pi with a Bluetooth PowerConf, a connected headset profile may
still deliver silent or corrupted microphone frames. Check for
`stage=wake state=detected` before debugging transcription or the agent. On one
Pi 5, CVSD (`bluez5.enable-msbc = false` in WirePlumber) plus basic SCO
(`options bluetooth disable_esco=1` in `/etc/modprobe.d/`) and 100% microphone
source volume restored wake detection. The SCO option affects all Bluetooth
headsets on that host and takes effect when the `bluetooth` module loads; test
the audio path before applying it to another machine.

Say `Hey Voice, turn off` to stop the listener without an LLM call; run
`voice on` to start it again.

While an agent task runs, Voice keeps listening instead of blocking capture on
the planner. It runs at most one agent request at a time: a busy turn can ask
for status, request cancellation, or acknowledge a correction that must be
resubmitted after the task completes. Cancellation cannot undo actions already
performed. Spoken replies are serialized with the thinking sound, and
microphone frames during playback and its hardware tail are discarded.

The extra wake-free window and cheap front desk are **off by default**. For a
short trial with a Sherpa wake detector, add these settings to the existing
config (keep its other fields unchanged):

```json
{
  "wake": {
    "in_flight_window": "25s",
    "in_flight_turns": 2
  },
  "brain": {
    "front": {
      "endpoint": "https://openrouter.ai/api/v1/chat/completions",
      "model": "openai/gpt-4.1-nano",
      "api_key_env": "OPENROUTER_API_KEY",
      "timeout": "2s"
    }
  }
}
```

These are fields within the existing `wake` and `brain` objects, not replacement
objects. Only a wake-authorized agent task opens the window; it closes on expiry,
the turn limit, completion, or shutdown. Outside it, another wake phrase is
required. The optional text model receives the accepted utterance and verified
busy status, never ambient audio, tools, action authority, or agent history. It
can choose a local reply or a bounded clarification question; invalid,
unverified, late, or unavailable responses fall back to local speech. Its
request has a 48-token response cap and usage/cost logging. Disabling
`brain.front` keeps the bounded local controls without front-model spend.

ALSA capture and playback are configurable argv templates. The current Pi deployment uses an Anker PowerConf. `packaging/99-voice-powerconf.rules` grants only `voice-agent` permission to send the USB telephony off-hook report required by that microphone.

### Microphone recovery

Enable `input.recovery` to supervise capture instead of terminating the listener
on a recoverable microphone failure. `input.fallbacks` also enables recovery
and lists additional capture commands in preference order, after the main
input. Every source uses the configured input sample rate and mono PCM format.

```json
"recovery": {
  "ready_timeout": "5s",
  "stall_timeout": "3s",
  "zero_timeout": "10s",
  "retry_interval": "2s",
  "preferred_interval": "30s"
},
"fallbacks": [
  {
    "device": "bluez_input.00_7F_1D_20_CC_F2.0",
    "command": ["sudo", "-n", "-u", "pi", "/usr/local/libexec/voice-bt-capture"],
    "output": {
      "device": "bluez_output.00_7F_1D_20_CC_F2.1",
      "command": ["sudo", "-n", "-u", "pi", "/usr/local/libexec/voice-bt-playback"]
    }
  }
]
```

These fields belong inside `input`. This PowerConf example requires the named
Bluetooth wrappers, a running user audio session, and scoped sudo permissions
on the host; it is not a portable Bluetooth installer. A paired fallback
`output` routes replies to that source's speaker, rather than leaving playback
pinned to a disconnected USB device. Omitting it retains the primary output.

For the deployed Pi, `packaging/voice-bt-connect` and
`packaging/voice-bt-capture` are host-specific helpers (owner `pi`, the MAC above).
Install them into `/usr/local/libexec` with mode 0755 when maintaining that host;
the existing `voice-bt-connect.timer` invokes the reconnect helper. It temporarily
blocks this paired Bluetooth device while `/proc/asound/PowerConf` exists and
unblocks/reconnects it when USB is removed. `Blocked: yes` with USB attached is
intentional; pairing is retained. Do not force both transports onto the same
speakerphone. A connected Bluetooth audio link was observed to stall USB capture.

The capture helper sets PipeWire `node.dont-fallback=true` and
`node.dont-reconnect=true`. A named `parec --device` alone is insufficient:
PipeWire can otherwise move a disappearing Bluetooth stream onto USB, falsely
reporting a healthy Bluetooth source and keeping ALSA USB capture busy.
Voice, not the audio server, must own microphone failover.

Hardware regression check: with USB attached and Bluetooth disconnected, the
Bluetooth capture helper must fail without recording from USB; direct USB
capture must still deliver full-length PCM. After restarting the listener,
confirm `stage=capture state=ready choice=0` and continued capture across timer
ticks. A physical USB-unplug test is needed to verify Bluetooth-only audio.
If the kernel repeatedly reports `SCO packet for unknown connection handle`,
a one-time Bluetooth controller power-off/on cleared the stuck transport on
this Pi; that interrupts all host Bluetooth devices and is not an automatic
recovery action. Also check Pi undervoltage warnings independently.

Readiness requires audio samples, not merely an open device. Missing frames,
capture exits, and persistent exactly-zero PCM trigger recovery; ordinary
quiet nonzero input is not treated as a dead microphone. The configured order
is never overwritten when a fallback is selected. A preferred source is
probed while fallback capture continues and is restored only at a quiet,
idle utterance boundary. Sources sharing a configured device or HID are not
probed concurrently. A broken stream discards an incomplete utterance rather
than joining speech from two microphones.

Omitted recovery timings use the values above. Ready, stall, and retry times
must be 100 ms–1 minute; zero timeout 1 second–10 minutes; preferred probe
interval 1 second–1 hour. There may be at most eight fallbacks. Leaving both
recovery and fallbacks absent retains the original single-device behavior.
`voice doctor` checks the capture executable, not live microphone health;
inspect the listener's capture health events to confirm usable input.

Speech engines and models are external. `scripts/setup-speech.sh` installs the Sherpa-ONNX keyword model, `whisper-cli`, the loopback `whisper-server`, Piper, and their default English models under `~/.local/share/voice`; the restricted installer copies them to `/var/lib/voice`. `voice-whisper.service` keeps the fallback Whisper model loaded once and binds only to `127.0.0.1:8178`.

### Personal vocabulary

`stt.vocabulary` supplies spelling hints and explicit corrections without a
second LLM pass or model training. It is optional; an empty configuration keeps
the original transcription behavior.

```json
"vocabulary": {
  "terms": ["PowerConf", "Pebble"],
  "replacements": [
    {"from": "power conf", "to": "PowerConf"}
  ],
  "context": "",
  "contexts": {
    "music": {
      "terms": ["Miles Davis"],
      "replacements": [{"from": "miles davies", "to": "Miles Davis"}]
    }
  }
}
```

Place this object inside `stt`. Set `context` to `"music"` to combine that
context's terms and corrections with the global ones. Context selection is
explicit configuration, not inferred from ambient audio, the screen, or the
clipboard. Leave it empty to use global terms only.

Corrections match whole terms case-insensitively, prefer the longest match at
the same position, and never cascade into another replacement. Targets must
be declared vocabulary terms. No fuzzy substitutions are made: unrelated
words, numbers, and negation remain unchanged unless an owner explicitly
configures a matching replacement. Review rules before using them for device
commands. Wake authorization uses the original transcript; a correction cannot
manufacture a wake phrase. Session diagnostics retain the raw recognition and
report corrected text separately.

The resident Whisper server receives the selected terms as a `prompt` hint.
A command engine receives them only where its argv includes `{vocabulary}`;
for Whisper CLI, append that placeholder to the existing `--prompt` value.
Other engines must explicitly support their own hint argument. OpenRouter
transcription does not receive unverified hint parameters; deterministic
corrections still apply after recognition. Hints guide recognition but do not
guarantee a particular spelling.

## Activation feedback

When a standalone wake phrase is accepted, Voice immediately switches the
speakerphone LED to a listening state and starts the acknowledgement sound. The
PowerConf receives a silent lead-in so its output path wakes before the sound;
the captured lead-in and sound are discarded rather than transcribed as the
follow-up command. Inline wake commands acknowledge after the whole utterance
is captured, so playback cannot mask the command.

Once a wake-authorized utterance closes, processing feedback begins before
transcription and remains active through routing, device work, and agent
planning. `thinking_delay` is a short grace period: requests that finish inside
it stay silent by construction. Processing playback is cancelled and joined
before response speech starts. The in-flight window adds no routine spoken
acknowledgement: the thinking sound continues while work is pending.
Status replies and other explicitly requested speech briefly
pause the loop; completion or cancellation stops it. Ambient noise, an embedded
mention, or an approximate wake phrase produce no feedback. The light returns
to idle on success, failure, timeout, cancellation, and shutdown.

```json
"feedback": {
  "light": "auto",
  "sound": "chime",
  "thinking": "tick",
  "thinking_delay": "250ms",
  "volume": 0.65
}
```

`light` names the indicator on the `input.telephony_hid` speakerphone: `auto`
takes the first LED the device's HID report descriptor advertises, `none`
disables it, or pick the one that reads as "listening" on your hardware with
`mic`, `ring`, `mute`, or `hold`. The off-hook LED is deliberately not
offered: capture holds it for the whole session to keep the microphone
un-gated. Indicator writes carry the complete HID output report, so lighting
up never clears that off-hook bit. A device whose descriptor advertises no
usable LED, or that cannot be opened, costs only the light: `voice doctor`
reports what was selected and why, and commands keep working.

`sound` is a generated waveform (`chime`, `blip`, `two-up`, or `none`) or a
WAV path. `thinking` is `tick`, `hum`, `none`, or a WAV path.
`thinking_delay` must be positive. `volume` scales acknowledgement playback
between 0 and 1; `thinking_volume` optionally controls processing playback
independently.

## Speech output

Piper is the offline speech engine. When `tts.openrouter` is configured and
its API key environment variable is present, Voice first asks
`openai/gpt-audio-mini` for streaming 24 kHz mono PCM16 speech through the
OpenRouter chat-completions endpoint. The shipped voice is `marin`.

The remote model is conversational rather than a strict narrator. Voice checks
the transcript returned with every audio stream against the requested words
before playing it. Added, omitted or changed words reject the remote audio and
fall back to Piper, as do missing credentials, timeouts and network failures.
The secret belongs in `/etc/voice/voice.env`, which the local installer creates
as `root:voice-agent` mode `0640`; it never belongs in `config.json`.

## Timers

Timers are local. "Hey Voice, set a timer for ten minutes" is parsed,
scheduled, and announced by the session layer, so a timer can be set and can
still fire when the model, the network, or the speech engines are unavailable —
the announcement is the only part that needs text-to-speech. Ask "how long is
left on my timer", cancel by duration ("cancel the ten minute timer") or in
bulk ("cancel all timers"). Anything the timer vocabulary does not recognise
goes to the agent unchanged.

```json
"timers": {
  "enabled": true
}
```

Deadlines are wall-clock, so a slow or suspended host does not shift when a
timer fires, and the set is persisted after every change. A timer set before a
restart still fires at its original time; one that came due while Voice was off
is reported as expired, with how long ago, exactly once.

State lives in the SQLite database `timers.db` under `$XDG_STATE_HOME/voice`,
falling back to `~/.local/state/voice`. The service unit sets
`StateDirectory=voice`, which puts it in `/var/lib/voice` owned by the
`voice-agent` account; `timers.file` overrides the path. Each save replaces the
set in one transaction, so a crash mid-write leaves the previous set rather
than half of the new one. A database that cannot be opened disables timers with
an explanation in the log instead of accepting requests it cannot keep.

The driver is `modernc.org/sqlite`, which is pure Go: the release binaries are
still `CGO_ENABLED=0` static builds for `amd64` and `arm64`. It is the only
third-party dependency, and it is not free — the static `arm64` binary grows
from 4.9 MB to 10.7 MB. That buys a transactional store the scheduler shares
with alarms and reminders instead of each of them reimplementing durable
replacement.

Because `voice off` stops the listener, it silences announcements without
destroying timers; `voice on` brings them back with the correct remaining time.

## Devices

Magic Home/LEDENET lights use their local TCP protocol on port 5577. HDMI-CEC TVs use the Linux CEC device. The OMP agent does not directly access hardware: it returns structured device actions, which Voice validates against configured IDs and capabilities before execution.

```sh
voice light <id> on
voice light <id> off
voice light <id> color purple
voice light <id> brightness 50
voice tv <id> on
voice tv <id> off
voice tv <id> volume-up
voice tv <id> volume-down
voice tv <id> mute
```

## Spotify

The optional Spotify integration installs an isolated playback daemon and adds
it to Voice's device inventory:

    scripts/install-spotify.sh

After authentication, spoken requests can start shuffled liked songs, resume or
pause playback, skip tracks, go back, play a named track, album, artist, or
playlist, and control music volume on an integer scale from 0 (silent) to 10
(maximum). “Turn the music up/down” moves one step. The equivalent direct
controls are:

    voice music spotify play
    voice music spotify play artist "Miles Davis"
    voice music spotify pause
    voice music spotify next
    voice music spotify volume 7
    voice music spotify volume-up

Spotify plays on the Creative Pebble V3 and **cannot reach the microphone**.
Voice can speak on the same speaker without stopping music: set
`output.command` to `["/usr/bin/sudo","-n","-H","-u","spotify-player","/usr/local/bin/voice-spotify-control","play-wav"]`.
The command accepts WAV bytes on stdin and uses the same ALSA dmix route as
Spotify. Voice's input can remain on the PowerConf USB or Bluetooth microphone.

Two independent guarantees, because one of them is only advice:

- **Which device.** `spotify-player` cannot name an output device - it calls
  `audio_backend::find(None)`, and librespot's ALSA backend maps that `None` to
  the string `"default"`. So `packaging/spotify/alsa.conf`, selected through
  `ALSA_CONFIG_PATH`, defines what `"default"` *means* for that process: the
  Creative Pebble V3, addressed as `card "V3"` and never as a card number,
  which shifts with probe order. Its ALSA dmix route shares playback with
  Voice's spoken replies. That file is **self-contained on purpose** -
  including `/usr/share/alsa/alsa.conf` made it decorative, because that file's
  trailing `@hooks` load `/etc/alsa/conf.d/*` *after* the root config is parsed
  and the PipeWire file there redefines `pcm.!default`.
- **Which devices are reachable at all.** The client runs as `spotify-player`,
  an account that is **not** in the `audio` group. A udev rule grants it access
  only to the Pebble's playback nodes and the global ALSA playback timer.
  Reaching another card - including the Voice capture device - fails with
  `EACCES`. A config a client can ignore is advice; a permission it cannot
  escape is a boundary.

Voice reaches the authenticated daemon through an allowlisted sudo bridge.
The daemon restarts even after a clean exit. A play request transfers playback
to the Pebble and waits for Spotify to report the new device; volume, pause,
and skip refuse to control a different active device, such as a phone.

The installer refuses to proceed unless it can demonstrate the ALSA namespace is
load-bearing, using a **paired control** - the same binary, the same invocation,
twice:

| namespace | required result |
|---|---|
| a bogus one naming a nonexistent card | playback **fails** |
| the real one | the identical command **succeeds** |

The pairing is what distinguishes "the namespace decides the device" from
"`aplay` is broken, absent, or the file is unreadable": a broken `aplay` fails
both arms, and an ignored `ALSA_CONFIG_PATH` passes both. If both arms fail the
installer reports `UNPROVEN` and stops, rather than concluding from the negative
alone. An ignored `ALSA_CONFIG_PATH` would otherwise look exactly like a working
one - audio would still play, just out of the wrong speaker.

Login is not automated. The installer prints the one command that needs a person,
because it opens a browser flow against the owner's Spotify account. Premium is
required for playback through this client.

## A custom wake sound

`feedback.sound` takes either a built-in name (`chime`, `blip`, `two-up`,
`none`) or a **path to a WAV file**:

```json
"feedback": { "sound": "/etc/voice/sounds/bubble.wav", "volume": 0.65 }
```

`feedback.thinking` takes a file the same way, for the sound played while an
answer is slow. That one LOOPS until the answer arrives, so the silence after
the sound belongs inside the file and is **kept** - it is the gap between
pulses, and trimming it would turn a tick into a rattle. A file shorter than
250 ms once converted is refused, because looping it restarts the player
continuously.

The reply does not wait for the cycle to finish: the loop is cancelled and
the playback process killed when the answer arrives, so a long file is cut
off rather than delaying speech. Device buffering can still leave a moment of
audible tail.

Both are loaded and checked when the config loads, so a missing or
undecodable file is reported at startup rather than discovered as silence the
first time someone speaks.

- **16-bit PCM WAV only.** Other encodings are refused with the `ffmpeg` line
  that converts them. Accepting a file and playing noise would be worse.
- **Stereo is mixed down and the rate is resampled** to the session format, so
  a file downloaded from any generator works without hand-conversion.
- **Trailing silence is trimmed for `feedback.sound` only.** Generators pad to
  round durations - a real example arrived as 0.504 s of file containing
  0.186 s of sound - and for a one-shot acknowledgement that padding holds the
  speaker open while the microphone is already listening for the command.
  `feedback.thinking` is the exception: it loops, so its padding is the gap
  between pulses and is kept.

## Feature proposals

“I cannot do that” loses the request. When Voice cannot do something — change
its own code, install software, anything needing the `pi` account — it records
a durable proposal instead: your request in your own words, its reading of
what you meant, a proposed scope and the material risks. It tells you it has
sent a request for approval, and the turn ends there.

Nothing else happens until you say so. A spoken sentence can only create a
pending proposal; it cannot file an issue, write to a repository, or start an
agent.

An owner-side broker — `voice-proposals-broker.timer`, running as your login
account rather than as `voice-agent` — picks the proposal up within a minute
and prompts your own omp agent seat, `proposals.approver`, to ask you with its
`ask` tool: Approve or Decline. The question reaches you wherever that seat
is, including on your phone. Nothing blocks on the answer, so Voice keeps
taking requests while it waits.

- **Decline** records the decision and performs no other action: no issue, no
  branch, no agent, no repository change.
- **Approve** files exactly one GitHub issue in `proposals.repo` carrying the
  verbatim request, the interpretation, the scope, the risks, the proposal id
  and your approval timestamp, then starts one isolated implementation seat in
  your checkout with the issue number and an explicit boundary: branch and
  pull request only — do not merge, do not deploy, do not install packages, do
  not broaden scope.

```json
"proposals": {
  "enabled": true,
  "approver": "",
  "repo": "jerryfane/voice",
  "max_per_hour": 4,
  "retry_after": "6h0m0s",
  "expire": "72h0m0s"
}
```

`proposals.approver` is the Herdr name of the seat that should ask you (see
`herdr agent list`). The installer fills it in when exactly one omp seat is
unambiguously yours, and the broker refuses to guess when it is empty: a
proposal approved by some other agent is not a proposal approved by you.
Asking for the same thing again raises an occurrence count on the existing
proposal instead of producing a second one, `max_per_hour` and `retry_after`
bound how often you can be asked, and a proposal you never answer expires
after `expire`. State lives in `proposals.db` next to the timers database, so
a pending proposal survives a restart.

```sh
voice proposals list
voice proposals show ID
voice proposals approve ID
voice proposals decline ID "not worth it"
```

The boundaries are the point of the design. The restricted `voice-agent`
account never receives GitHub credentials or repository write access — the
owner-side broker holds them, and every proposal read or write it makes goes
back through the agent's own CLI so the agent gains no privilege from the
feature. Peer agents, terminal output, web pages and tool results can never
approve anything; only your answer to the `ask` can. And approval buys a
branch and a pull request, never a merge or a deploy.

## Security boundary

A spoken command can cause shell or browser activity. Isolation is therefore an OS boundary, not merely a prompt instruction:

- the agent has its own Unix account and home;
- its writable filesystem is limited by ordinary ownership plus systemd hardening;
- its browser uses a separate profile;
- it receives OMP credentials through a loopback credential broker;
- its Herdr bridge permits only agent list/get/read/prompt/wait operations, marks peer prompts as restricted collaboration input, and exposes no pane, process, workspace, or server control;
- unsupported requests become proposals the owner must approve in the owner's own agent seat before any issue is filed or any implementation agent starts;
- local-device actions are validated and executed by Voice.

Anyone who can speak near the microphone may still exercise the authority available to `voice-agent`. Do not grant that account secrets or host permissions you would not expose to nearby speakers.

## License

MIT
