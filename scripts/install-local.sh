#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$HOME/.local/bin/voiced" ./cmd/voiced
cp packaging/voiced.service "$HOME/.config/systemd/user/voiced.service"
systemctl --user daemon-reload

if [ -f packaging/99-voiced-powerconf.rules ] && command -v sudo >/dev/null 2>&1; then
  sudo install -m 0644 packaging/99-voiced-powerconf.rules /etc/udev/rules.d/99-voiced-powerconf.rules
  sudo udevadm control --reload-rules
  sudo udevadm trigger
fi

echo "Installed ~/.local/bin/voiced and the voiced user service."
echo "Run: voiced init && voiced doctor"
echo "Enable after configuration: systemctl --user enable --now voiced"
