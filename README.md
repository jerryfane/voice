# Voice

A persistent, tool-capable voice agent for Linux. Say “Hey Voice,” speak a request, and continue one long-lived OMP conversation with its own workspace and memory.

```
microphone → VAD → local Whisper → “Hey Voice” → persistent OMP agent
                                                      ├─ shell and files
                                                      ├─ browser and web search
                                                      ├─ Magic Home lights
                                                      ├─ HDMI-CEC television
                                                      └─ local Piper speech
```

## Agent model

Nontrivial requests run in one persistent OMP session. The session:

- works in `/home/voice-agent/voice-workspace`;
- retains conversation history across voice requests;
- keeps durable facts in `MEMORY.md`;
- follows the dedicated account’s OMP default model;
- can use shell, read/write, browser, and web-search tools;
- runs as the restricted `voice-agent` OS account, not as your login account;
- cannot read the `pi` account’s private files or authenticated browser profile.

Raw audio never goes to the model. Recording, VAD, Whisper transcription, wake matching, Piper synthesis, and device execution remain local. After a wake match, the recognized text and configured device inventory are sent to the OMP model.

## Install

Install speech dependencies and the restricted system service from source:

```sh
./scripts/setup-speech.sh
./scripts/install-local.sh
```

The installer creates `voice-agent`, installs the system services, copies OMP into the restricted runtime, configures its credential-broker connection, and starts Voice.

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

User-mode configuration defaults to `~/.config/voice/config.json`; override it with `VOICE_CONFIG` or `--config PATH`.

## Use

```sh
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

## Audio and wake phrase

The default wake phrase is exactly:

```text
Hey Voice
```

ALSA capture and playback are configurable argv templates. The current Pi deployment uses an Anker PowerConf. `packaging/99-voice-powerconf.rules` grants only `voice-agent` permission to send the USB telephony off-hook report required by that microphone.

Speech engines and models are external. `scripts/setup-speech.sh` installs whisper.cpp, Piper, and their default English models under `~/.local/share/voice`; the restricted installer copies them to `/var/lib/voice`.

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

## Security boundary

A spoken command can cause shell or browser activity. Isolation is therefore an OS boundary, not merely a prompt instruction:

- the agent has its own Unix account and home;
- its writable filesystem is limited by ordinary ownership plus systemd hardening;
- its browser uses a separate profile;
- it receives OMP credentials through a loopback credential broker;
- it cannot attach to an existing terminal or agent session;
- local-device actions are validated and executed by Voice.

Anyone who can speak near the microphone may still exercise the authority available to `voice-agent`. Do not grant that account secrets or host permissions you would not expose to nearby speakers.

## License

MIT
