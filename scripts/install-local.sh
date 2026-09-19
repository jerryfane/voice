#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."
owner=$(id -un)
owner_home=$HOME
agent_user=voice-agent
agent_home=/home/voice-agent
workspace=$agent_home/voice-workspace
start_service=${VOICE_INSTALL_START:-1}
herdr_bin=$(command -v herdr 2>/dev/null || true)
if [ "$herdr_bin" = /usr/local/bin/herdr ] && [ -x /usr/local/libexec/voice-herdr-real ]; then
  herdr_bin=/usr/local/libexec/voice-herdr-real
fi

run_as_agent() {
  sudo -u "$agent_user" env HOME="$agent_home" sh -c 'cd "$1"; shift; exec "$@"' sh "$workspace" "$@"
}

command -v sudo >/dev/null 2>&1 || { echo "sudo is required" >&2; exit 1; }
command -v omp >/dev/null 2>&1 || { echo "omp is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
[ -n "$herdr_bin" ] || { echo "herdr is required" >&2; exit 1; }


CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /tmp/voice-install ./cmd/voice
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tmp/voice-herdr-report-install ./cmd/voice-herdr-report


if ! getent passwd "$agent_user" >/dev/null; then
  sudo useradd --system --user-group --create-home --home-dir "$agent_home" --shell /bin/bash "$agent_user"
fi
sudo install -d -o "$agent_user" -g "$agent_user" -m 0750 "$workspace" "$agent_home/.local/share/voice/sessions" "$agent_home/.local/share/voice/responses" "$agent_home/.omp/agent" "$agent_home/.omp/agent/extensions" "$agent_home/.omp/agent/skills" "$agent_home/.omp/agent/skills/herdr" /var/cache/voice /var/lib/voice
sudo chown -R "$agent_user:$agent_user" "$agent_home"
sudo install -d -o root -g root -m 0755 /usr/local/libexec /etc/voice
sudo install -m 0755 /tmp/voice-install /usr/local/bin/voice
sudo install -m 0755 /tmp/voice-herdr-report-install /usr/local/libexec/voice-herdr-report
sherpa_module=$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)
case "$(uname -m)" in
  aarch64|arm64) sherpa_arch=aarch64-unknown-linux-gnu ;;
  x86_64|amd64) sherpa_arch=x86_64-unknown-linux-gnu ;;
  *) echo "unsupported Sherpa-ONNX architecture: $(uname -m)" >&2; exit 1 ;;
esac
sudo install -d -o root -g root -m 0755 /usr/local/lib/voice
sudo install -m 0644 "$sherpa_module/lib/$sherpa_arch/"*.so /usr/local/lib/voice/
printf '%s\n' /usr/local/lib/voice | sudo tee /etc/ld.so.conf.d/voice.conf >/dev/null
sudo ldconfig
sudo install -m 0755 "$(command -v omp)" /usr/local/libexec/voice-omp
printf '%s ALL=(voice-agent) NOPASSWD: SETENV: /usr/local/bin/voice-agent-run *, /usr/local/bin/voice-agent-session *\nvoice-agent ALL=(%s) NOPASSWD: /usr/local/libexec/voice-herdr *, /usr/local/bin/voice-herdr-ensure\n%s ALL=(root) NOPASSWD: /usr/bin/systemctl start voice.service, /usr/bin/systemctl stop voice.service\n' "$owner" "$owner" "$owner" | sudo tee /etc/sudoers.d/voice-agent >/dev/null
sudo chmod 0440 /etc/sudoers.d/voice-agent
sudo visudo -cf /etc/sudoers.d/voice-agent >/dev/null
printf '%s\n' "$owner" | sudo tee /etc/voice/herdr-owner >/dev/null
sudo chmod 0644 /etc/voice/herdr-owner
sudo install -m 0755 "$herdr_bin" /usr/local/libexec/voice-herdr-real
sudo install -m 0755 packaging/voice-herdr /usr/local/libexec/voice-herdr
sudo install -m 0755 packaging/voice-herdr /usr/local/bin/herdr
sudo install -m 0755 packaging/voice-herdr-ensure /usr/local/bin/voice-herdr-ensure
sudo install -m 0755 packaging/voice-agent-session /usr/local/bin/voice-agent-session
sudo install -m 0755 packaging/voice-agent-run /usr/local/bin/voice-agent-run
sudo install -o "$agent_user" -g "$agent_user" -m 0644 packaging/voice-response.ts "$agent_home/.omp/agent/extensions/voice-response.ts"
"$herdr_bin" --skill > /tmp/voice-herdr-skill.md
sudo install -o "$agent_user" -g "$agent_user" -m 0644 /tmp/voice-herdr-skill.md "$agent_home/.omp/agent/skills/herdr/SKILL.md"
rm -f /tmp/voice-herdr-skill.md
sudo install -m 0644 packaging/voice.service /etc/systemd/system/voice.service
sudo install -m 0644 packaging/voice-whisper.service /etc/systemd/system/voice-whisper.service
sudo install -m 0644 packaging/voice-auth-broker@.service /etc/systemd/system/voice-auth-broker@.service
sudo install -m 0644 packaging/99-voice-powerconf.rules /etc/udev/rules.d/99-voice-powerconf.rules
sudo install -m 0644 packaging/AGENTS.md "$workspace/AGENTS.md"
if [ ! -e /etc/voice/voice.env ]; then
  sudo install -o root -g "$agent_user" -m 0640 /dev/null /etc/voice/voice.env
fi
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
if [ -x "$owner_home/.local/bin/whisper-server" ]; then
  sudo install -m 0755 "$owner_home/.local/bin/whisper-server" /usr/local/bin/whisper-server
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
  wake_filter='.wake.phrases = ["hey voice", "hey boys"] | .wake.fuzz = 0.2 | del(.wake.follow_up)'
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
  .wake.detector = \"sherpa\" |
  .wake.sherpa = {\"encoder\":\"/var/lib/voice/models/sherpa-kws/encoder.int8.onnx\",\"decoder\":\"/var/lib/voice/models/sherpa-kws/decoder.onnx\",\"joiner\":\"/var/lib/voice/models/sherpa-kws/joiner.int8.onnx\",\"tokens\":\"/var/lib/voice/models/sherpa-kws/tokens.txt\",\"keywords\":\"/var/lib/voice/models/sherpa-kws/keywords.txt\",\"num_threads\":1,\"max_active_paths\":4,\"keywords_score\":4,\"keywords_threshold\":0.15} |
  .wake.vad.max_utterance = \"6s\" |
  .wake.vad.pre_roll = \"1.5s\" |
  .stt.model = \"/var/lib/voice/models/ggml-base.en.bin\" |
  .stt.resident //= {\"endpoint\":\"http://127.0.0.1:8178/inference\",\"timeout\":\"10s\"} |
  .stt.openrouter //= {\"endpoint\":\"https://openrouter.ai/api/v1/audio/transcriptions\",\"model\":\"openai/whisper-large-v3-turbo\",\"api_key_env\":\"OPENROUTER_API_KEY\",\"timeout\":\"5s\"} |
  .tts.model = \"/var/lib/voice/models/en_US-amy-medium.onnx\" |
  .brain.mode = \"agent\" |
  .brain.command = [\"voice-agent-run\", \"{prompt}\"] |
  .brain.timeout = \"10m0s\" |
  .brain.persona = \"You are Voice, a concise personal agent. Complete the user request with your tools, remember useful context across turns, and answer in one or two natural spoken sentences.\" |
  .brain.jev //= {\"endpoint\":\"https://openrouter.ai/api/alpha/decisions\",\"model\":\"typesafe/jev-1.13\",\"api_key_env\":\"OPENROUTER_API_KEY\",\"confidence\":0.85,\"timeout\":\"2s\"} |
  if .brain.jev.endpoint == \"https://openrouter.ai/api/v1/api/alpha/decisions\" then .brain.jev.endpoint = \"https://openrouter.ai/api/alpha/decisions\" else . end"
sudo jq "$config_filter" /etc/voice/config.json > /tmp/voice-config.json

# Belt and braces, because the thing being overwritten is the only copy of
# the owner's configuration: refuse to install a config the binary itself
# will not accept.
#
# The check is the BINARY, not a second copy of the rules. Keeping validation
# in config.Validate prevents the installer and runtime from accepting
# different detector configurations.
if [ ! -s /tmp/voice-config.json ] || ! /usr/local/bin/voice --config /tmp/voice-config.json config > /dev/null; then
  rm -f /tmp/voice-config.json
  echo "refusing to install a config the voice binary rejects" >&2
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
if ! /usr/local/bin/voice-herdr-ensure; then
  echo "warning: Herdr voice agent is unavailable; local commands will still work" >&2
fi


sudo systemctl daemon-reload
sudo udevadm control --reload-rules
sudo udevadm trigger
sudo systemctl enable "voice-auth-broker@$owner.service"
sudo systemctl restart "voice-auth-broker@$owner.service"
sudo systemctl enable voice-whisper.service
sudo systemctl restart voice-whisper.service
sudo systemctl enable voice.service
if [ "$start_service" = 1 ]; then
  # restart, not enable --now: --now starts a STOPPED unit and leaves a running
  # one alone, so an install must explicitly load the new binary.
  sudo systemctl restart voice.service
else
  # Silent maintenance mode: install everything but leave the microphone and
  # speaker process stopped until the owner explicitly starts it.
  sudo systemctl stop voice.service
fi
rm -f /tmp/voice-install /tmp/voice-herdr-report-install

installed_hash=$(sha256sum /usr/local/bin/voice | cut -d' ' -f1)
echo "Installed /usr/local/bin/voice with restricted agent account $agent_user ($installed_hash)."
if [ "$start_service" = 1 ]; then
  # Verify the process now running IS the file just installed, rather than
  # trusting the restart.
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
  echo "Verified pid $main_pid is running that exact binary."
else
  echo "voice.service remains stopped for silent maintenance."
fi
echo "Workspace: $workspace"
echo "Service: systemctl status voice"
