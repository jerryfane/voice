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
printf '%s ALL=(voice-agent) NOPASSWD: /usr/local/bin/voice-agent-run *\n%s ALL=(root) NOPASSWD: /usr/bin/systemctl start voice.service, /usr/bin/systemctl stop voice.service\n' "$owner" "$owner" | sudo tee /etc/sudoers.d/voice-agent >/dev/null
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

fresh_config=0
if [ ! -f /etc/voice/config.json ]; then
  sudo /usr/local/bin/voice --config /etc/voice/config.json init
  fresh_config=1
fi

# The wake settings belong to whoever owns the speaker, not to this script.
# It used to assign wake.phrases and wake.fuzz on EVERY run, which silently
# removed the owner's "hey boys" alias each time the device was reinstalled -
# restored by hand afterwards, every time, by the person doing the install.
# A deploy step that quietly undoes a user's configuration is a defect even
# when the value it writes is the documented default.
wake_filter='.'
if [ "$fresh_config" = 1 ]; then
  wake_filter='.wake.phrases = ["hey voice"] | .wake.fuzz = 0 | del(.wake.follow_up)'
else
  printf 'keeping existing wake phrases: %s\n' "$(sudo jq -c '.wake.phrases' /etc/voice/config.json)"
fi

# ONE jq, not a pipeline. A pipeline made this step unable to fail: the
# shebang is `env sh`, which is dash here, dash has no pipefail, and `set -e`
# in a pipeline only inspects the LAST command's status. With an unparseable
# config - a bad manual edit, an unclean power loss on an embedded device -
# the first jq died, the second produced an empty document, and a 0-byte
# config was then installed OVER the working one. The single-jq form this
# replaced aborted instead. Introduced by me and caught in review.
config_filter="$wake_filter |
  .stt.model = \"/var/lib/voice/models/ggml-base.en.bin\" |
  .tts.model = \"/var/lib/voice/models/en_US-amy-medium.onnx\" |
  .brain.mode = \"agent\" |
  .brain.command = [\"voice-agent-run\", \"{prompt}\"] |
  .brain.timeout = \"10m0s\" |
  .brain.persona = \"You are Voice, a concise personal agent. Complete the user request with your tools, remember useful context across turns, and answer in one or two natural spoken sentences.\" |
  .brain.rules = false"
sudo jq "$config_filter" /etc/voice/config.json > /tmp/voice-config.json

# Belt and braces, because the thing being overwritten is the only copy of
# the owner's configuration: refuse to install a file that is empty or does
# not parse, whatever the exit statuses said.
if [ ! -s /tmp/voice-config.json ] || ! jq -e 'type == "object" and (.wake.phrases | length > 0)' /tmp/voice-config.json > /dev/null; then
  rm -f /tmp/voice-config.json
  echo "refusing to install a config that is empty, unparseable, or has no wake phrases" >&2
  exit 1
fi

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
sudo systemctl enable "voice-auth-broker@$owner.service"
sudo systemctl restart "voice-auth-broker@$owner.service"
sudo systemctl enable voice.service
# restart, not enable --now: --now starts a STOPPED unit and leaves a running
# one alone, so every install since the per-stage logging landed wrote a new
# binary that nobody then ran. The device served the household from a build
# months behind the repo while this script printed "Installed".
sudo systemctl restart voice.service
rm -f /tmp/voice-install

# Verify the process now running IS the file just installed, rather than
# trusting the restart. A restart can be refused, a unit can fail and leave the
# old process up, and a service can be masked; an installer that reports
# success without checking is how the stale binary survived every reinstall.
installed_hash=$(sha256sum /usr/local/bin/voice | cut -d' ' -f1)
main_pid=$(systemctl show -p MainPID --value voice.service)
if [ -z "$main_pid" ] || [ "$main_pid" = "0" ]; then
  echo "install failed: voice.service is not running after restart" >&2
  exit 1
fi
running_exe=$(sudo readlink -f "/proc/$main_pid/exe" 2>/dev/null || true)
if [ -z "$running_exe" ]; then
  echo "install failed: cannot read /proc/$main_pid/exe to confirm what is running" >&2
  exit 1
fi
running_hash=$(sudo sha256sum "$running_exe" | cut -d' ' -f1)
if [ "$installed_hash" != "$running_hash" ]; then
  echo "install failed: the running program is not the one just installed." >&2
  echo "  installed /usr/local/bin/voice: $installed_hash" >&2
  echo "  running   $running_exe (pid $main_pid): $running_hash" >&2
  exit 1
fi

echo "Installed /usr/local/bin/voice with restricted agent account $agent_user."
echo "Verified pid $main_pid is running that exact binary ($installed_hash)."
echo "Workspace: $workspace"
echo "Service: systemctl status voice"
