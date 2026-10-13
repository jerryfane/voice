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
voice phone <id> read
voice phone <id> open-app <configured-alias>
voice phone <id> volume-up
voice phone <id> volume-down
voice phone <id> pause
voice phone <id> resume
voice phone <id> dial <number>
voice say "text to speak"
```

Always use IDs returned by `voice devices`. Never guess an address or CEC adapter.

Android is opt-in and requires an already-authorized connected ADB target,
explicit serial, and owner-allowlisted operations/app aliases. Never invoke ADB
or arbitrary phone tools to bypass Voice's allowlist. The persistent Voice
planner must return structured actions rather than invoking these CLI commands.
Only perform the user's requested phone task; ask for missing/ambiguous aliases
or numbers, never infer numbers or read contacts. `dial` accepts a string of
1–15 digits with optional leading `+` and only opens the dialer: the user must
confirm the call on the phone. `read` checks connection, not private content.
Receipts acknowledge requests, not verified app/media state or completed calls.
No messages, private-data reads, arbitrary shell/taps/text, or lock bypass.
The allowlist is an API boundary, not an OS sandbox around the ADB server.

## Persistent agent

```sh
voice ask "remember that I prefer concise answers"
```

The agent workspace is `/home/voice-agent/voice-workspace`. Its OMP conversation continues across requests, and durable facts belong in `MEMORY.md`. The agent can use shell, file, web-search, and browser tools, but OS permissions isolate it from the `pi` account.

## Configuration

User-mode default: `~/.config/voice/config.json`, overridden by `VOICE_CONFIG` or `--config PATH`. The restricted system service uses `/etc/voice/config.json`.
