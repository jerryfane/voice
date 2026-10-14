package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
)

func phoneAgentCLIFixture(t *testing.T, enabled bool) (string, *bytes.Buffer, *bytes.Buffer, []byte) {
	t.Helper()
	dir := t.TempDir()
	var screen bytes.Buffer
	if err := png.Encode(&screen, image.NewRGBA(image.Rect(0, 0, 17, 31))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "screen.png"), screen.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" != '-s' ] || [ "$2" != 'cli-test-serial' ]; then exit 91; fi
shift 2
case "$1" in
  exec-out)
    /bin/cat "$PHONE_CLI_SCREEN"
    if [ "$PHONE_CLI_FAIL" = 1 ]; then printf 'private permission denied\n' >&2; exit 19; fi ;;
  shell)
    if [ "$PHONE_CLI_UI_FORBIDDEN" = 1 ]; then : > "$PHONE_CLI_UI_MARKER"; exit 94; fi
    if [ "$2" != '-T' ]; then exit 92; fi
    printf '%s' '<hierarchy><node text="Private screen"/></hierarchy>' ;;
  *) exit 93 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("PHONE_CLI_SCREEN", filepath.Join(dir, "screen.png"))
	t.Setenv("PHONE_CLI_FAIL", "")
	t.Setenv("PHONE_CLI_UI_FORBIDDEN", "")
	t.Setenv("PHONE_CLI_UI_MARKER", filepath.Join(dir, "unexpected-ui"))
	cfg := config.Default()
	cfg.Android = &config.Android{ID: "pocket", Serial: "cli-test-serial", AgentControl: enabled}
	// No capture or playback binary exists: phone tools must not need either.
	cfg.Input.Command = config.Command{"missing-phone-test-microphone"}
	cfg.Output.Command = config.Command{"missing-phone-test-speaker"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	savedOut, savedErr := stdout, stderr
	stdout, stderr = &printer{w: &out}, &printer{w: &errout}
	t.Cleanup(func() { stdout, stderr = savedOut, savedErr })
	return path, &out, &errout, screen.Bytes()
}

func TestPhoneAgentCLIObservationJSONWithoutAudio(t *testing.T) {
	for _, mode := range []string{"", "--screenshot", "--both"} {
		t.Run(mode, func(t *testing.T) {
			path, out, errout, want := phoneAgentCLIFixture(t, true)
			args := []string{"--config", path, "phone", "pocket", "observe"}
			if mode != "" {
				args = append(args, mode)
			}
			if mode == "" {
				t.Setenv("PHONE_CLI_FAIL", "1")
			} else if mode == "--screenshot" {
				t.Setenv("PHONE_CLI_UI_FORBIDDEN", "1")
			}
			if err := run(args); err != nil {
				t.Fatal(err)
			}
			var observation device.AndroidObservation
			if err := json.Unmarshal(out.Bytes(), &observation); err != nil {
				t.Fatalf("observe is not plain machine-readable JSON: %q %v", out, err)
			}
			if observation.Kind != "voice.android.observation" || len(observation.Warnings) != 0 || errout.Len() != 0 {
				t.Fatalf("observation mixed with diagnostics or receipt: %+v %q", observation, errout)
			}
			if mode == "--screenshot" {
				if observation.UI != nil {
					t.Fatalf("screenshot-only returned UI: %+v", observation)
				}
				if _, err := os.Stat(os.Getenv("PHONE_CLI_UI_MARKER")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("screenshot-only started UI automation: %v", err)
				}
			} else if observation.UI == nil || len(observation.UI.Elements) != 1 || observation.UI.Elements[0].Text != "Private screen" {
				t.Fatalf("compact UI unavailable: %+v", observation)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if fields["ui_hierarchy"] != nil {
				t.Fatal("public observation returned raw XML")
			}
			if mode == "" {
				if fields["screenshot_path"] != nil {
					t.Fatal("default observation returned a screenshot path")
				}
			} else {
				got, err := os.ReadFile(observation.ScreenshotPath)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("CLI screenshot is not original full PNG: %v", err)
				}
			}
		})
	}
}

func TestPhoneAgentCLIDisabledAndBoundaryErrors(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		path, out, errout, _ := phoneAgentCLIFixture(t, enabled)
		cases := [][]string{
			{"pocket", "observe", "extra"}, {"pocket", "adb"},
			{"pocket", "observe", "--screenshot", "--both"}, {"pocket", "observe", "--raw"},
			{"pocket", "observe", "--both", "--both"},
			{"pocket", "adb", "-s", "different-phone", "shell", "id"},
			{"missing", "observe"},
		}
		if !enabled {
			cases = append(cases, []string{"pocket", "observe"}, []string{"pocket", "observe", "--screenshot"}, []string{"pocket", "observe", "--both"}, []string{"pocket", "adb", "exec-out", "screencap", "-p"})
		}
		for _, args := range cases {
			if err := run(append([]string{"--config", path, "phone"}, args...)); err == nil {
				t.Fatalf("accepted invalid or disabled tool call: %v", args)
			}
			if out.Len() != 0 || errout.Len() != 0 {
				t.Fatalf("denied tool call emitted output: %q %q", out, errout)
			}
		}
	}
}
