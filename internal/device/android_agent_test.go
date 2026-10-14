package device

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

func generalAndroid() *Android {
	return NewAndroid(config.Android{ID: "phone", Serial: "PINNED-PHONE", AgentControl: true})
}

// This fixture provides binary screen captures and hierarchy failure modes, not
// success receipts: tests exercise PNG/XML validation, persistence and deadlines.
func androidObservationFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	state := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 13, 29))
	img.SetNRGBA(12, 28, color.NRGBA{R: 231, G: 70, B: 99, A: 255})
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "screen.png")
	if err := os.WriteFile(path, pngBytes.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" != '-s' ] || [ "$2" != 'PINNED-PHONE' ]; then exit 90; fi
shift 2
case "$1" in
  exec-out)
    if [ "$2" != screencap ] || [ "$3" != '-p' ] || [ "$#" != 3 ]; then exit 91; fi
    if [ "$PHONE_SCREEN_FAIL" = 1 ]; then printf 'screen access denied' >&2; exit 7; fi
    /bin/cat "$PHONE_SCREEN_FIXTURE" ;;
  shell)
    if [ "$2" != '-T' ] || [ "$3" != sh ] || [ "$4" != '-c' ]; then exit 92; fi
    case "$PHONE_UI_MODE" in
      fail) printf 'UI automation unavailable' >&2; exit 8 ;;
      delay) exec /bin/sleep 30 ;;
      malformed) printf '<hierarchy><node' ;;
      empty) exit 0 ;;
      *) printf '%s' '<?xml version="1.0"?><hierarchy rotation="0"><node text="Private chat &amp; name" bounds="[0,0][13,29]"/></hierarchy>' ;;
    esac ;;
  *) exit 93 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PHONE_SCREEN_FIXTURE", path)
	t.Setenv("PHONE_SCREEN_FAIL", "")
	t.Setenv("PHONE_UI_MODE", "")
	return pngBytes.Bytes(), filepath.Join(state, "voice", "android")
}

func TestAndroidAgentGateBeforeProcessOrState(t *testing.T) {
	log := fakeAndroidADB(t)
	state := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("XDG_STATE_HOME", state)
	for _, cfg := range []config.Android{
		{ID: "phone", Serial: "PINNED-PHONE"},
		{ID: "phone", Serial: "-s other", AgentControl: true},
	} {
		d := NewAndroid(cfg)
		if _, err := d.ADB(context.Background(), []string{"shell", "input", "tap", "1", "2"}); err == nil {
			t.Fatal("general command allowed without valid opt-in config")
		}
		if got, err := d.Observe(context.Background()); err == nil || got.ScreenshotPath != "" {
			t.Fatalf("observation allowed without valid opt-in config: %+v %v", got, err)
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("denied general command invoked ADB: %q", calls)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied observation touched state directory: %v", err)
	}
}

func TestAndroidAgentPreventsTransportOverrides(t *testing.T) {
	log := fakeAndroidADB(t)
	for _, args := range [][]string{
		nil, {""}, {"-s", "other", "shell", "id"}, {"-sOTHER", "shell", "id"},
		{"-d", "shell", "id"}, {"-e", "shell", "id"}, {"-t", "2", "shell", "id"},
		{"-H", "other", "shell", "id"}, {"-P", "9999", "shell", "id"}, {"-L", "tcp:other:5037", "shell", "id"},
		{"--one-device", "other", "start-server"}, {"--", "-s", "other", "shell"},
		{"connect", "other"}, {"disconnect"}, {"pair", "other"}, {"reconnect", "offline"},
		{"attach", "other"}, {"detach", "other"}, {"start-server"}, {"kill-server"}, {"server", "nodaemon"},
		{"wait-for-device", "-s", "other", "shell", "id"}, {"shell", "invalid\x00arg"},
	} {
		if _, err := generalAndroid().ADB(context.Background(), args); err == nil {
			t.Fatalf("accepted host transport override: %q", args)
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("invalid command reached ADB: %s", calls)
	}
}

func TestAndroidAgentCancelledCommandDoesNotReachPhone(t *testing.T) {
	log := fakeAndroidADB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := generalAndroid().ADB(ctx, []string{"shell", "id"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled raw command: %v", err)
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("cancelled command reached the phone: %s", calls)
	}
}

func TestAndroidObserveFullPrivateLatestArtifacts(t *testing.T) {
	_, dir := androidObservationFixture(t)
	for frame := range 3 {
		// Different complete frames must replace, not reuse or crop, the
		// previous capture while keeping retained artifacts bounded.
		img := image.NewNRGBA(image.Rect(0, 0, 13+frame, 29+frame))
		img.SetNRGBA(12+frame, 28+frame, color.NRGBA{R: 231, G: uint8(frame * 70), A: 255})
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal(err)
		}
		want := encoded.Bytes()
		if err := os.WriteFile(os.Getenv("PHONE_SCREEN_FIXTURE"), want, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := generalAndroid().Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.ScreenshotPath != filepath.Join(dir, "screen.png") || !strings.Contains(got.UIHierarchy, "Private chat &amp; name") || len(got.Warnings) != 0 {
			t.Fatalf("incomplete observation: %+v", got)
		}
		data, err := os.ReadFile(got.ScreenshotPath)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("screenshot changed/truncated: %v", err)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("unbounded or missing observation artifacts: %v %v", files, err)
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private artifact mode: %v, %v", info, err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode: %v, %v", info, err)
	}
}

func TestAndroidObserveUIFailureRetainsScreenshot(t *testing.T) {
	for _, mode := range []string{"fail", "malformed", "empty", "delay"} {
		t.Run(mode, func(t *testing.T) {
			want, _ := androidObservationFixture(t)
			t.Setenv("PHONE_UI_MODE", mode)
			started := time.Now()
			got, err := generalAndroid().Observe(context.Background())
			if err != nil || got.UIHierarchy != "" || len(got.Warnings) != 1 {
				t.Fatalf("UI failure discarded screenshot or claimed hierarchy: %+v, %v", got, err)
			}
			if time.Since(started) > androidUITimeout+3*time.Second {
				t.Fatal("UI timeout was not bounded")
			}
			data, err := os.ReadFile(got.ScreenshotPath)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatalf("UI failure lost valid screenshot: %v", err)
			}
		})
	}
}

func TestAndroidObserveCaptureFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"adb-failure", "not-png", "truncated-png", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			pngData, dir := androidObservationFixture(t)
			ctx := context.Background()
			switch mode {
			case "adb-failure":
				t.Setenv("PHONE_SCREEN_FAIL", "1")
			case "not-png", "truncated-png":
				data := []byte("error: permission denied")
				if mode == "truncated-png" {
					data = pngData[:len(pngData)-10]
				}
				if err := os.WriteFile(os.Getenv("PHONE_SCREEN_FIXTURE"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := generalAndroid().Observe(ctx)
			if err == nil || got.ScreenshotPath != "" {
				t.Fatalf("failed capture claimed success: %+v %v", got, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "screen.png")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed capture left a success artifact: %v", err)
			}
		})
	}
}

func TestAndroidObserveCallerDeadlineIsNotUIWarning(t *testing.T) {
	_, _ = androidObservationFixture(t) // discard: Fixture bytes and paths are unused; this case checks only cancellation.
	t.Setenv("PHONE_UI_MODE", "delay")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err := generalAndroid().Observe(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || got.ScreenshotPath != "" {
		t.Fatalf("caller deadline reported success: %+v %v", got, err)
	}
}

func TestAndroidArtifactsRejectSymlinksAndLockCancellation(t *testing.T) {
	_, dir := androidObservationFixture(t)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := generalAndroid().Observe(context.Background()); err == nil {
		t.Fatal("followed symlinked private state directory")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	unlock, err := androidObservationLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unlock(); err != nil {
			t.Errorf("release observation lock: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := androidObservationLock(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			if err := release(); err != nil {
				t.Errorf("release unexpected lock: %v", err)
			}
		}
		t.Fatalf("contended observation lock ignored deadline: %v", err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "screen.png.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := androidSaveScreenshot(filepath.Join(dir, "screen.png"), []byte("replace")); err == nil {
		t.Fatal("followed staging-file symlink")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("symlink victim changed: %q %v", data, err)
	}
}

func TestAndroidHierarchyRejectsMissingOrMisleadingXML(t *testing.T) {
	for _, input := range []string{"", "UI hierchary dumped", "<node/>", "<hierarchy>", "<hierarchy/><hierarchy/>", "error< hierarchy/>", "<hierarchy/>extra"} {
		if got, err := androidHierarchy([]byte(input)); err == nil || got != "" {
			t.Fatalf("accepted invalid hierarchy %q: %q %v", input, got, err)
		}
	}
}

func TestAndroidGeneralModeLeavesLegacyPermissionsIndependent(t *testing.T) {
	log := fakeAndroidADB(t)
	d := generalAndroid()
	if _, err := d.Apply(context.Background(), Command{Op: OpOpenApp, Args: map[string]any{"app": "unlisted"}}); err == nil {
		t.Fatal("general control implicitly broadened the structured app command")
	}
	if _, err := d.Read(context.Background()); err == nil {
		t.Fatal("general control implicitly granted legacy read")
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("denied structured commands reached ADB: %q", calls)
	}
	if _, err := d.ADB(context.Background(), []string{"shell", "am", "start", "-n", "unlisted.app/.Activity"}); err != nil {
		t.Fatalf("general mode incorrectly applied legacy app/operation permissions: %v", err)
	}
}
