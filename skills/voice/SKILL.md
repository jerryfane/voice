---
name: voice
description: Control and inspect the local Voice assistant, its persistent agent, lights, HDMI-CEC television, audio devices, and speech pipeline.
compatibility: Linux host with the voice binary; optional ALSA audio, cec-ctl, Magic Home devices, whisper.cpp, Piper, and OMP.
metadata:
  author: jerryfane
  version: "0.3.0"
---

# Voice

Use the installed `voice` binary. Voice wakes on “Hey Voice” and sends nontrivial requests to one persistent, tool-capable OMP session owned by the restricted `voice-agent` account.

## Check readiness

```sh
voice doctor
voice devices
systemctl status voice
```

## Direct deterministic actions

```sh
voice light <id> on
voice light <id> off
voice light <id> toggle
voice light <id> color <red|green|blue|purple|orange|yellow|cyan|pink|white>
voice light <id> white <0-100>
voice light <id> brightness <0-100>
voice tv <id> on
voice tv <id> off
voice tv <id> volume-up
voice tv <id> volume-down
voice tv <id> mute
voice say "text to speak"
```

Always use IDs returned by `voice devices`. Never guess an address or CEC adapter.

## Persistent agent

```sh
voice ask "remember that I prefer concise answers"
```

The agent workspace is `/home/voice-agent/voice-workspace`. Its OMP conversation continues across requests, and durable facts belong in `MEMORY.md`. The agent can use shell, file, web-search, and browser tools, but OS permissions isolate it from the `pi` account.

## Configuration

User-mode default: `~/.config/voice/config.json`, overridden by `VOICE_CONFIG` or `--config PATH`. The restricted system service uses `/etc/voice/config.json`.
