#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$HOME/.local/bin/herdr" ./cmd/herdr
cp packaging/herdr.service "$HOME/.config/systemd/user/herdr.service"
systemctl --user daemon-reload

if [ -f packaging/99-herdr-powerconf.rules ] && command -v sudo >/dev/null 2>&1; then
  sudo install -m 0644 packaging/99-herdr-powerconf.rules /etc/udev/rules.d/99-herdr-powerconf.rules
  sudo udevadm control --reload-rules
  sudo udevadm trigger
fi

echo "Installed ~/.local/bin/herdr and the herdr user service."
echo "Run: herdr init && herdr doctor"
echo "Enable after configuration: systemctl --user enable --now herdr"
