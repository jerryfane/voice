---
name: voiced
description: Control local lights and HDMI-CEC televisions, inspect devices, speak responses, or run the Herdr Voice assistant through the installed voiced binary. Use for deterministic smart-home actions and local voice interaction.
compatibility: Linux host with the voiced binary; optional ALSA audio, cec-ctl, Magic Home devices, whisper.cpp, Piper, and OMP.
metadata:
  author: jerryfane
  version: "0.2.0"
---

# Voiced — Herdr Voice runtime

Use the installed `voiced` binary. The `herdr` command belongs to the terminal-workspace federation and MUST NOT be invoked for voice actions. Never synthesize protocol packets yourself when a `voiced` CLI operation exists.

## Check readiness

```sh
voiced doctor
voiced devices
```

`doctor` is the authoritative dependency and permission check. Report failures exactly; do not claim a microphone, speaker, speech model, light, or TV works unless its check is `OK`.

## Deterministic actions

Prefer these commands over `voiced ask` because they do not invoke a model:

```sh
voiced light <id> on
voiced light <id> off
voiced light <id> toggle
voiced light <id> color <red|green|blue|purple|orange|yellow|cyan|pink|white>
voiced light <id> white <0-100>
voiced light <id> brightness <0-100>
voiced tv <id> on
voiced tv <id> off
voiced tv <id> volume-up
voiced tv <id> volume-down
voiced tv <id> mute
voiced say "text to speak"
```

Always use IDs returned by `voiced devices`. Never guess an address or CEC adapter.

## Natural language

```sh
voiced ask "turn the bedroom light purple"
```

This uses the configured planner, validates its JSON plan against the device registry, performs actions, and speaks the response.

## Voice loop

```sh
voiced listen
```

This is long-running and belongs under `voiced.service`, not a foreground agent shell. It captures audio only after the configured device opens, applies VAD, transcribes candidate utterances, and reacts only after a wake phrase.

## Configuration

Default path: `~/.config/voiced/config.json`; override with `VOICED_CONFIG` or `--config PATH`. Initialize with `voiced init`. Never commit a user's live config because it contains private device addresses and command choices.
