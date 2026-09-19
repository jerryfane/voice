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

Audio is always wake-gated locally by a resident 3M-parameter Sherpa-ONNX keyword spotter. Only a bounded utterance accepted after `HEY_VOICE` or `HEY_BOYS` is detected can reach OpenRouter; ambient and non-wake audio is discarded without transcription. Provider failure falls back to the loopback resident Whisper server and then `whisper-cli`.

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
`whisper-cli` remain ordered local fallbacks. A standalone wake phrase is
handled locally and does not call the agent.

Say `Hey Voice, turn off` to stop the listener without an LLM call; run
`voice on` to start it again.

ALSA capture and playback are configurable argv templates. The current Pi deployment uses an Anker PowerConf. `packaging/99-voice-powerconf.rules` grants only `voice-agent` permission to send the USB telephony off-hook report required by that microphone.

Speech engines and models are external. `scripts/setup-speech.sh` installs the Sherpa-ONNX keyword model, `whisper-cli`, the loopback `whisper-server`, Piper, and their default English models under `~/.local/share/voice`; the restricted installer copies them to `/var/lib/voice`. `voice-whisper.service` keeps the fallback Whisper model loaded once and binds only to `127.0.0.1:8178`.

## Activation feedback

When the exact wake phrase is accepted, Voice acknowledges it locally before
the request reaches the agent: a speakerphone LED switches to a listening
state, and a short generated tone plays once. Both are emitted by the session
layer, so they cost no model call and work with no network. Ambient noise, an
embedded mention, or an approximate phrase produce neither. The light returns
to idle on success, failure, timeout, cancellation, and shutdown.

```json
"feedback": {
  "light": "auto",
  "sound": "chime",
  "volume": 0.35
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

`sound` is a generated waveform, not a bundled asset: `chime`, `blip`,
`two-up`, or `none`. `volume` scales it between 0 and 1.

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

Spotify plays to a second speaker and **cannot reach the microphone**.

Two independent guarantees, because one of them is only advice:

- **Which device.** `spotify-player` cannot name an output device - it calls
  `audio_backend::find(None)`, and librespot's ALSA backend maps that `None` to
  the string `"default"`. So `packaging/spotify/alsa.conf`, selected through
  `ALSA_CONFIG_PATH`, defines what `"default"` *means* for that process: the
  Creative Pebble V3, addressed as `card "V3"` and never as a card number,
  which shifts with probe order. That file is **self-contained on purpose** -
  including `/usr/share/alsa/alsa.conf` made it decorative, because that file's
  trailing `@hooks` load `/etc/alsa/conf.d/*` *after* the root config is parsed
  and the PipeWire file there redefines `pcm.!default`.
- **Which devices are reachable at all.** The client runs as `spotify-player`,
  an account that is **not** in the `audio` group. A udev rule grants it access
  to the Pebble's nodes alone, matched by USB vendor and product. Reaching any
  other card - including the capture device the Voice service holds - fails with
  `EACCES`. A config a client can ignore is advice; a permission it cannot
  escape is a boundary.

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

## Security boundary

A spoken command can cause shell or browser activity. Isolation is therefore an OS boundary, not merely a prompt instruction:

- the agent has its own Unix account and home;
- its writable filesystem is limited by ordinary ownership plus systemd hardening;
- its browser uses a separate profile;
- it receives OMP credentials through a loopback credential broker;
- its Herdr bridge permits only agent list/get/read/prompt/wait operations, marks peer prompts as restricted collaboration input, and exposes no pane, process, workspace, or server control;
- local-device actions are validated and executed by Voice.

Anyone who can speak near the microphone may still exercise the authority available to `voice-agent`. Do not grant that account secrets or host permissions you would not expose to nearby speakers.

## License

MIT
