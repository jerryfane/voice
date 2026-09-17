#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."
owner=$(id -un)
owner_home=$HOME
agent_user=voice-agent
agent_home=/home/voice-agent
workspace=$agent_home/voice-workspace
run_as_agent() {
  sudo -u "$agent_user" env HOME="$agent_home" sh -c 'cd "$1"; shift; exec "$@"' sh "$workspace" "$@"
}

command -v sudo >/dev/null 2>&1 || { echo "sudo is required" >&2; exit 1; }
command -v omp >/dev/null 2>&1 || { echo "omp is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tmp/voice-install ./cmd/voice

if ! getent passwd "$agent_user" >/dev/null; then
  sudo useradd --system --user-group --create-home --home-dir "$agent_home" --shell /bin/bash "$agent_user"
fi
sudo install -d -o "$agent_user" -g "$agent_user" -m 0750 "$workspace" "$agent_home/.local/share/voice/sessions" "$agent_home/.omp/agent" /var/cache/voice /var/lib/voice
sudo chown -R "$agent_user:$agent_user" "$agent_home"
sudo install -d -o root -g root -m 0755 /usr/local/libexec /etc/voice
sudo install -m 0755 /tmp/voice-install /usr/local/bin/voice
sudo install -m 0755 "$(command -v omp)" /usr/local/libexec/voice-omp
printf '%s ALL=(voice-agent) NOPASSWD: /usr/local/bin/voice-agent-run *\n' "$owner" | sudo tee /etc/sudoers.d/voice-agent >/dev/null
sudo chmod 0440 /etc/sudoers.d/voice-agent
sudo visudo -cf /etc/sudoers.d/voice-agent >/dev/null
sudo install -m 0755 packaging/voice-agent-run /usr/local/bin/voice-agent-run
sudo install -m 0644 packaging/voice.service /etc/systemd/system/voice.service
sudo install -m 0644 packaging/voice-auth-broker@.service /etc/systemd/system/voice-auth-broker@.service
sudo install -m 0644 packaging/99-voice-powerconf.rules /etc/udev/rules.d/99-voice-powerconf.rules
sudo install -m 0644 packaging/AGENTS.md "$workspace/AGENTS.md"
if [ ! -e "$workspace/MEMORY.md" ]; then
  sudo -u "$agent_user" touch "$workspace/MEMORY.md"
fi

if [ -d "$owner_home/.local/share/voice" ]; then
  sudo cp -a "$owner_home/.local/share/voice/." /var/lib/voice/
  sudo chown -R "$agent_user:$agent_user" /var/lib/voice
fi
if [ -x "$owner_home/.local/bin/whisper-cli" ]; then
  sudo install -m 0755 "$owner_home/.local/bin/whisper-cli" /usr/local/bin/whisper-cli
fi
if [ -x /var/lib/voice/piper/piper ]; then
  sudo ln -sfn /var/lib/voice/piper/piper /usr/local/bin/piper
fi

if [ ! -f /etc/voice/config.json ]; then
  sudo /usr/local/bin/voice --config /etc/voice/config.json init
fi
sudo jq '
  .wake.phrases = ["hey voice"] |
  .wake.fuzz = 0.2 |
  .stt.model = "/var/lib/voice/models/ggml-base.en.bin" |
  .tts.model = "/var/lib/voice/models/en_US-amy-medium.onnx" |
  .brain.mode = "agent" |
  .brain.command = ["voice-agent-run", "{prompt}"] |
  .brain.timeout = "10m0s" |
  .brain.persona = "You are Voice, a concise personal agent. Complete the user request with your tools, remember useful context across turns, and answer in one or two natural spoken sentences." |
  .brain.rules = false
' /etc/voice/config.json > /tmp/voice-config.json
sudo install -o root -g "$agent_user" -m 0640 /tmp/voice-config.json /etc/voice/config.json
rm -f /tmp/voice-config.json
sudo chown root:"$agent_user" /etc/voice/config.json
sudo setfacl -m "u:$owner:r" /etc/voice/config.json
sudo chmod 0640 /etc/voice/config.json

if [ -f "$owner_home/.omp/agent/config.yml" ]; then
  sudo install -o "$agent_user" -g "$agent_user" -m 0600 "$owner_home/.omp/agent/config.yml" "$agent_home/.omp/agent/config.yml"
fi
token=$(omp auth-broker token)
run_as_agent /usr/local/libexec/voice-omp config set auth.broker.url http://127.0.0.1:9444 >/dev/null
run_as_agent /usr/local/libexec/voice-omp config set auth.broker.token "$token" >/dev/null

sudo systemctl daemon-reload
sudo udevadm control --reload-rules
sudo udevadm trigger
sudo systemctl enable --now "voice-auth-broker@$owner.service"
sudo systemctl enable --now voice.service
rm -f /tmp/voice-install

echo "Installed /usr/local/bin/voice with restricted agent account $agent_user."
echo "Workspace: $workspace"
echo "Service: systemctl status voice"
