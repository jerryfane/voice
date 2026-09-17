---
name: herdr
description: Control local lights and HDMI-CEC televisions, inspect devices, speak responses, or run the Herdr voice assistant through the installed herdr binary. Use for deterministic smart-home actions and local voice interaction.
compatibility: Linux host with the herdr binary; optional ALSA audio, cec-ctl, Magic Home devices, whisper.cpp, Piper, and OMP.
metadata:
  author: jerryfane
  version: "0.1.0"
---

# Herdr

Use the installed `herdr` binary. Never synthesize protocol packets yourself when a CLI operation exists.

## Check readiness

```sh
herdr doctor
herdr devices
```

`doctor` is the authoritative dependency and permission check. Report failures exactly; do not claim a microphone, speaker, speech model, light, or TV works unless its check is `OK`.

## Deterministic actions

Prefer these commands over `herdr ask` because they do not invoke a model:

```sh
herdr light <id> on
herdr light <id> off
herdr light <id> toggle
herdr light <id> color <red|green|blue|purple|orange|yellow|cyan|pink|white>
herdr light <id> white <0-100>
herdr light <id> brightness <0-100>
herdr tv <id> on
herdr tv <id> off
herdr tv <id> volume-up
herdr tv <id> volume-down
herdr tv <id> mute
herdr say "text to speak"
```

Always use IDs returned by `herdr devices`. Never guess an address or CEC adapter.

## Natural language

```sh
herdr ask "turn the bedroom light purple"
```

This uses the configured planner, validates its JSON plan against the device registry, performs actions, and speaks the response.

## Voice loop

```sh
herdr listen
```

This is long-running and belongs under the provided systemd user service, not a foreground agent shell. It captures audio only after the configured device opens, applies VAD, transcribes candidate utterances, and reacts only after a wake phrase.

## Configuration

Default path: `~/.config/herdr/config.json`; override with `HERDR_CONFIG` or `--config PATH`. Initialize with `herdr init`. Never commit a user's live config because it contains private device addresses and command choices.
