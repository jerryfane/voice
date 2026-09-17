# Herdr Voice

A local, open-source voice runtime for agent harnesses. The installed command and service are named `voiced`; `herdr` is intentionally reserved for the separate Herdr terminal-workspace federation.

Say a wake phrase, speak a command, let an agent reason, hear the answer, and control local devices. Voiced is one statically linked Go binary. It does not own your microphone, speaker, speech model, or agent provider: each boundary is configurable, so any device or engine that exposes a command-line interface works.

```
microphone → VAD → speech-to-text → wake phrase → rules / OMP → actions
                                                               ├─ Magic Home lights
                                                               ├─ HDMI-CEC television
                                                               └─ spoken reply → TTS → speaker
```

## Properties

- **One binary:** pure Go, no CGO, no Node or Python runtime.
- **Any audio hardware:** capture/playback are argv templates. Defaults use ALSA `arecord` and `aplay`; PipeWire, FFmpeg, remote audio, or custom programs work too.
- **Any agent:** defaults to `omp -p`; replace it with Claude, a local model, an HTTP wrapper, or a script.
- **Any speech engine:** defaults to whisper.cpp and Piper, both local. Engines are external and replaceable.
- **Local-first devices:** Magic Home/LEDENET lights use TCP 5577 directly. TVs use Linux HDMI-CEC. Neither requires a cloud account.
- **Fast path:** time, date, common power/color/volume commands bypass the model.
- **Safe actions:** the model returns a strict JSON plan. Voiced validates every device ID and capability before execution. Configured commands are argv arrays, never shell strings.

## Install

Download the latest release:

```sh
curl -fsSL https://raw.githubusercontent.com/jerryfane/herdr-voice/main/install.sh | sh
voiced init
voiced doctor
```

Or build from source (Go 1.24+):

```sh
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/voiced ./cmd/voiced
./scripts/setup-speech.sh
voiced init
```

`setup-speech.sh` builds whisper.cpp and installs Piper plus the default English models under `~/.local/share/voiced/models`. They are not linked into Voiced; edit the JSON config to use other engines.

Configuration defaults to `~/.config/voiced/config.json`. Override it with `VOICED_CONFIG` or `--config PATH`.

### Upgrading from v0.1

v0.1 incorrectly installed the voice runtime as `~/.local/bin/herdr`. Restore the federation binary before upgrading, then install v0.2 or newer as `voiced`. Move only the voice file `~/.config/herdr/config.json` to `~/.config/voiced/config.json`; do not move or delete the surrounding `~/.config/herdr` directory because the Herdr federation owns its other files. Move voice models from `~/.local/share/herdr` to `~/.local/share/voiced`, update absolute model paths, and replace the old voice unit with `voiced.service`.

## Configure audio

List ALSA devices:

```sh
arecord -l   # microphones
aplay -l     # speakers
```

Set `input.device` and `output.device`, for example:

```json
{
  "input": {
    "device": "plughw:2,0",
    "sample_rate": 16000,
    "telephony_hid": "/dev/hidraw5"
  },
  "output": { "device": "plughw:2,0" }
}
```

The optional `telephony_hid` handles USB speakerphones that stream silence until the host reports an active call. `packaging/99-voiced-powerconf.rules` grants the service permission for Anker PowerConf (`291a:3301`). Other microphones omit it.

Commands may use placeholders documented in `internal/config/config.go`: `{device}`, `{rate}`, `{channels}`, `{file}`, `{model}`, `{text}`, `{out}`, and `{prompt}`.

## Configure devices

Magic Home/LEDENET light:

```json
"lights": [
  { "id": "bedroom", "host": "192.168.1.50", "protocol": "9byte" }
]
```

Give controllers static DHCP leases. The protocol is local TCP port 5577.

HDMI-CEC television:

```json
"tvs": [
  { "id": "tv", "adapter": "/dev/cec0", "logical_address": 0 }
]
```

Check the bus with `cec-ctl -d /dev/cec0 --playback --to 0 --give-device-power-status`.

## Use

```sh
voiced doctor
voiced devices
voiced ask 'turn the bedroom light purple'
voiced light bedroom white 60
voiced tv tv off
voiced say 'Herdr is ready.'
voiced listen
```

Default wake spellings include “hey herdr”, “hey herder”, and likely STT variants. Recognition is fuzzy and configurable. The default detector transcribes only VAD-accepted speech segments; custom wake engines can be substituted without changing the binary.

## Agent skill

`skills/voiced/SKILL.md` packages the runtime as an agentskills.io-compatible skill. Agents should call deterministic CLI operations directly (`voiced light`, `voiced tv`, `voiced devices`) and use `voiced ask` only for natural-language planning.

## Service

```sh
mkdir -p ~/.config/systemd/user
cp packaging/voiced.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now voiced
```

Install the optional PowerConf udev rule as root, then replug the device:

```sh
sudo cp packaging/99-voiced-powerconf.rules /etc/udev/rules.d/
sudo udevadm control --reload-rules
sudo udevadm trigger
```

## Status

Supported today:

- Linux audio through command adapters
- energy VAD with ambient auto-calibration
- whisper.cpp STT and Piper TTS adapters
- fuzzy wake-phrase matching and follow-up window
- OMP-backed JSON planning and no-model local rules
- Magic Home RGB(W) lights
- HDMI-CEC TV power, mute, and volume
- Anker PowerConf USB telephony handshake

Planned drivers should implement `internal/device.Device`; planned engines implement the contracts under `internal/speech` or `internal/audio`.

## License

MIT
