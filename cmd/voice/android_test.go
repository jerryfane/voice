package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/session"
)

func phoneCLIFixture(t *testing.T, operations []string) (*session.Assistant, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "adb-calls")
	script := `#!/bin/sh
printf '%s' "$1" >> "$VOICE_ANDROID_CLI_LOG"
shift
for arg do
  printf '\t%s' "$arg" >> "$VOICE_ANDROID_CLI_LOG"
done
printf '\n' >> "$VOICE_ANDROID_CLI_LOG"
if [ "$1" != "cli-test-serial" ]; then
  exit 1
fi
shift
case "$1" in
  get-state) printf 'device\n' ;;
  shell)
    case "$2" in
      am) printf 'Status: ok\n' ;;
      input) exit 0 ;;
      *) exit 1 ;;
    esac ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VOICE_ANDROID_CLI_LOG", logPath)
	cfg := config.Config{Android: &config.Android{
		ID:         "pocket",
		Serial:     "cli-test-serial",
		Operations: operations,
		Apps:       map[string]string{"maps": "com.example.maps"},
	}}
	var out bytes.Buffer
	saved := stdout
	stdout = &printer{w: &out}
	t.Cleanup(func() { stdout = saved })
	return &session.Assistant{Devices: device.Build(cfg)}, &out, logPath
}

func TestPhoneCLIRejectsArgumentBoundariesBeforeADB(t *testing.T) {
	cases := [][]string{
		nil,
		{"pocket"},
		{"pocket", "open-app"},
		{"pocket", "open-app", "maps", "extra"},
		{"pocket", "open_app", "maps", "extra"},
		{"pocket", "dial"},
		{"pocket", "dial", "+12025550123", "extra"},
		{"pocket", "read", "extra"},
		{"pocket", "volume-up", "extra"},
		{"pocket", "volume_down", "extra"},
		{"pocket", "pause", "extra"},
		{"pocket", "resume", "extra"},
		{"pocket", "call", "+12025550123"},
		{"pocket", "shell", "id"},
		{"pocket", "unknown", "extra"},
		{"missing", "read"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			a, out, logPath := phoneCLIFixture(t, []string{"read", "open_app", "dial", "volume_up", "volume_down", "pause", "resume"})
			if err := commandDevice(context.Background(), a, "phone", args); err == nil {
				t.Fatal("invalid arguments were accepted")
			}
			if out.Len() != 0 {
				t.Fatalf("invalid arguments printed success: %s", out)
			}
			if calls, err := os.ReadFile(logPath); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments reached adb: calls=%q err=%v", calls, err)
			}
		})
	}
}

func TestPhoneCLIHonorsOwnerPermissionsAndKind(t *testing.T) {
	for _, operations := range [][]string{nil, {"read"}} {
		a, out, logPath := phoneCLIFixture(t, operations)
		for _, args := range [][]string{{"pocket", "open-app", "maps"}, {"pocket", "dial", "+12025550123"}, {"pocket", "volume-up"}, {"pocket", "volume-down"}, {"pocket", "pause"}, {"pocket", "resume"}} {
			if err := commandDevice(context.Background(), a, "phone", args); err == nil {
				t.Fatalf("unadvertised operation accepted: %v", args)
			}
		}
		if operations == nil {
			if err := commandDevice(context.Background(), a, "phone", []string{"pocket", "read"}); err == nil {
				t.Fatal("empty allowlist permitted read")
			}
		}
		if err := commandDevice(context.Background(), a, "tv", []string{"pocket", "volume-up"}); err == nil {
			t.Fatal("phone accepted as TV")
		}
		if out.Len() != 0 {
			t.Fatalf("rejected operation printed success: %s", out)
		}
		if calls, err := os.ReadFile(logPath); !os.IsNotExist(err) {
			t.Fatalf("rejected operation reached adb: calls=%q err=%v", calls, err)
		}
	}
}
