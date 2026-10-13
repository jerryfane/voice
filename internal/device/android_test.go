package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

func androidTestConfig() config.Android {
	return config.Android{
		ID: "phone", Serial: "PINNED-PHONE",
		Operations: []string{"read", "open_app", "dial", "volume_up", "volume_down", "pause", "resume"},
		Apps:       map[string]string{"maps": "com.example.maps", "music": "com.example.music"},
	}
}

// PATH contains only this fake, never a real adb. Arguments are logged one per
// line so tests also catch unexpected argument boundaries or target fallback.
func fakeAndroidADB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	body := `#!/bin/sh
printf 'BEGIN\n' >> "$ANDROID_TEST_LOG"
printf '%s\n' "$@" >> "$ANDROID_TEST_LOG"
if [ "$1" != '-s' ] || [ "$2" != 'PINNED-PHONE' ]; then
  exit 90
fi
if [ "$3" = 'get-state' ]; then
  if [ -n "$ANDROID_TEST_CHECK_DELAY" ]; then /bin/sleep "$ANDROID_TEST_CHECK_DELAY"; fi
  printf '%s\n' "$ANDROID_TEST_STATE"
  printf '%s' "$ANDROID_TEST_CHECK_STDERR" >&2
  exit "$ANDROID_TEST_CHECK_EXIT"
fi
if [ -n "$ANDROID_TEST_ACTION_DELAY" ]; then exec /bin/sleep "$ANDROID_TEST_ACTION_DELAY"; fi
printf '%s\n' "$ANDROID_TEST_OUTPUT"
printf '%s' "$ANDROID_TEST_STDERR" >&2
exit "$ANDROID_TEST_EXIT"
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ANDROID_TEST_LOG", log)
	t.Setenv("ANDROID_TEST_STATE", "device")
	t.Setenv("ANDROID_TEST_OUTPUT", "Starting: Intent { private phone output }\nStatus: ok\nComplete")
	for _, name := range []string{"ANDROID_TEST_CHECK_EXIT", "ANDROID_TEST_EXIT"} {
		t.Setenv(name, "0")
	}
	for _, name := range []string{"ANDROID_TEST_CHECK_DELAY", "ANDROID_TEST_ACTION_DELAY", "ANDROID_TEST_CHECK_STDERR", "ANDROID_TEST_STDERR"} {
		t.Setenv(name, "")
	}
	return log
}

func androidCalls(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func androidWantCall(args ...string) string {
	return "BEGIN\n-s\nPINNED-PHONE\n" + strings.Join(args, "\n") + "\n"
}

func TestAndroidCommandsPinnedAndReceiptOnly(t *testing.T) {
	tests := []struct {
		cmd  Command
		args []string
	}{
		{Command{Op: OpOpenApp, Args: map[string]any{"app": "maps"}}, []string{"shell", "am", "start", "-W", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", "com.example.maps"}},
		{Command{Op: OpDial, Args: map[string]any{"number": "+123456789012345"}}, []string{"shell", "am", "start", "-W", "-a", "android.intent.action.DIAL", "-d", "tel:+123456789012345"}},
		{Command{Op: OpVolumeUp}, []string{"shell", "input", "keyevent", "24"}},
		{Command{Op: OpVolumeDown}, []string{"shell", "input", "keyevent", "25"}},
		{Command{Op: OpPause}, []string{"shell", "input", "keyevent", "127"}},
		{Command{Op: OpResume}, []string{"shell", "input", "keyevent", "126"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.cmd.Op), func(t *testing.T) {
			log := fakeAndroidADB(t)
			cfg := androidTestConfig()
			// Mutation connection checks are not a grant of the read capability.
			cfg.Operations = []string{string(tt.cmd.Op)}
			d := NewAndroid(cfg)
			state, err := d.Apply(context.Background(), tt.cmd)
			if err != nil {
				t.Fatal(err)
			}
			receipt, ok := state["receipt"].(string)
			if len(state) != 1 || !ok || receipt == "" {
				t.Fatalf("expected request-only receipt, got %v", state)
			}
			for _, private := range []string{cfg.Serial, "123456789012345", "private phone output"} {
				if strings.Contains(receipt, private) {
					t.Fatalf("receipt leaked private value: %q", receipt)
				}
			}
			want := androidWantCall("get-state") + androidWantCall(tt.args...)
			if calls := androidCalls(t, log); calls != want || strings.Contains(calls, "android.intent.action.CALL") {
				t.Fatalf("calls = %q, want %q", calls, want)
			}
		})
	}
}

func TestAndroidReadIsExplicitAndConnectionOnly(t *testing.T) {
	log := fakeAndroidADB(t)
	d := NewAndroid(androidTestConfig())
	for _, read := range []func() (State, error){
		func() (State, error) { return d.Read(context.Background()) },
		func() (State, error) { return d.Apply(context.Background(), Command{Op: OpRead}) },
	} {
		state, err := read()
		if err != nil || !reflect.DeepEqual(state, State{"state": "connected"}) {
			t.Fatalf("read = %v, %v", state, err)
		}
	}
	if calls := androidCalls(t, log); calls != strings.Repeat(androidWantCall("get-state"), 2) {
		t.Fatalf("read issued unexpected commands: %q", calls)
	}
}

func TestAndroidRejectsCommandsBeforeAnyProcess(t *testing.T) {
	tests := []Command{
		{Op: OpOn}, {Op: Op("shell")}, {Op: Op("call")}, {Op: Op("tap")}, {Op: Op("text")},
		{Op: OpRead, Args: map[string]any{"private": true}},
		{Op: OpOpenApp},
		{Op: OpOpenApp, Args: map[string]any{"app": 1}},
		{Op: OpOpenApp, Args: map[string]any{"app": "unknown"}},
		{Op: OpOpenApp, Args: map[string]any{"app": "com.example.maps"}},
		{Op: OpOpenApp, Args: map[string]any{"app": "maps;id"}},
		{Op: OpOpenApp, Args: map[string]any{"app": "maps", "component": "com.example/.Main"}},
		{Op: OpDial},
		{Op: OpDial, Args: map[string]any{"number": 123}},
		{Op: OpDial, Args: map[string]any{"number": "123", "confirm": true}},
	}
	for _, op := range []Op{OpVolumeUp, OpVolumeDown, OpPause, OpResume} {
		tests = append(tests, Command{Op: op, Args: map[string]any{"serial": "other"}})
	}
	for _, number := range []string{"", "+", "++123", "1234567890123456", "+1234567890123456", "12 34", "123\n", "123;id", "$(id)", "123&call", "123,456", "123#", "*123", "tel:123", "１２３", "123-456", "123/456", "123%0a"} {
		tests = append(tests, Command{Op: OpDial, Args: map[string]any{"number": number}})
	}
	log := fakeAndroidADB(t)
	d := NewAndroid(androidTestConfig())
	for _, cmd := range tests {
		state, err := d.Apply(context.Background(), cmd)
		if err == nil || state != nil {
			t.Fatalf("accepted invalid command %+v: %v, %v", cmd, state, err)
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("invalid command ran adb: %q", calls)
	}
}

func TestAndroidEmptyAndRestrictedAllowlists(t *testing.T) {
	log := fakeAndroidADB(t)
	for _, ops := range [][]string{nil, {}, {"open_app"}} {
		cfg := androidTestConfig()
		cfg.Operations = ops
		d := NewAndroid(cfg)
		if len(d.Capabilities()) != len(ops) {
			t.Fatalf("capabilities = %v, want %v", d.Capabilities(), ops)
		}
		if _, err := d.Read(context.Background()); err == nil {
			t.Fatal("Read granted without read allowlist, including doctor path")
		}
		for _, op := range []Op{OpRead, OpDial, OpVolumeUp, OpVolumeDown, OpPause, OpResume} {
			if _, err := d.Apply(context.Background(), Command{Op: op}); err == nil {
				t.Fatalf("operation %q granted without permission", op)
			}
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("denied operation ran adb: %q", calls)
	}
}

func TestAndroidDirectConstructionRejectsInvalidConfig(t *testing.T) {
	log := fakeAndroidADB(t)
	for _, modify := range []func(*config.Android){
		func(c *config.Android) { c.ID = "" },
		func(c *config.Android) { c.Serial = "" },
		func(c *config.Android) { c.Serial = "serial;id" },
		func(c *config.Android) { c.Serial = "$(id)" },
		func(c *config.Android) { c.Serial = "-d" },
		func(c *config.Android) { c.Apps["maps"] = "com.example;id" },
		func(c *config.Android) { c.Apps["maps;id"] = "com.example" },
		func(c *config.Android) { c.Operations = []string{"read", "shell"} },
		func(c *config.Android) { c.Operations = []string{"read", "read"} },
	} {
		cfg := androidTestConfig()
		modify(&cfg)
		d := NewAndroid(cfg)
		if len(d.Capabilities()) != 0 {
			t.Fatal("invalid config advertised capabilities")
		}
		if _, err := d.Read(context.Background()); err == nil {
			t.Fatal("invalid config allowed read")
		}
		if _, err := d.Apply(context.Background(), Command{Op: OpOpenApp, Args: map[string]any{"app": "maps"}}); err == nil {
			t.Fatal("invalid config allowed mutation")
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("invalid config ran adb: %q", calls)
	}
}

func TestAndroidConnectionFailuresNeverMutate(t *testing.T) {
	for _, state := range []string{"unauthorized", "offline", "unknown", "", "device\nunauthorized", "devices", "recovery"} {
		t.Run(state, func(t *testing.T) {
			log := fakeAndroidADB(t)
			t.Setenv("ANDROID_TEST_STATE", state)
			d := NewAndroid(androidTestConfig())
			got, err := d.Apply(context.Background(), Command{Op: OpPause})
			if err == nil || got != nil {
				t.Fatalf("unavailable phone succeeded: %v, %v", got, err)
			}
			if calls := androidCalls(t, log); calls != androidWantCall("get-state") {
				t.Fatalf("unavailable phone received mutation: %q", calls)
			}
		})
	}
}

func TestAndroidErrorsDoNotExposeProcessOutput(t *testing.T) {
	for _, phase := range []string{"check", "mutation"} {
		t.Run(phase, func(t *testing.T) {
			log := fakeAndroidADB(t)
			private := "PINNED-PHONE tel:+123456789 private data"
			if phase == "check" {
				t.Setenv("ANDROID_TEST_CHECK_EXIT", "1")
				t.Setenv("ANDROID_TEST_CHECK_STDERR", private)
			} else {
				t.Setenv("ANDROID_TEST_EXIT", "1")
				t.Setenv("ANDROID_TEST_STDERR", private)
				t.Setenv("ANDROID_TEST_OUTPUT", private)
			}
			state, err := NewAndroid(androidTestConfig()).Apply(context.Background(), Command{Op: OpDial, Args: map[string]any{"number": "+123456789"}})
			if err == nil || state != nil {
				t.Fatalf("failed adb reported success: %v, %v", state, err)
			}
			for _, secret := range []string{"PINNED-PHONE", "123456789", "private data"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("private error leaked: %v", err)
				}
			}
			if phase == "check" && androidCalls(t, log) != androidWantCall("get-state") {
				t.Fatal("connection process failure still mutated phone")
			}
		})
	}
}

func TestAndroidRejectsMisleadingActivityExitSuccess(t *testing.T) {
	for _, output := range []struct{ stdout, stderr string }{
		{"", ""},
		{"Starting: Intent { private }", ""},
		{"Status: timeout", ""},
		{"Error: Activity not started", ""},
		{"Status: ok\nError: private phone output", ""},
		{"Status: ok\njava.lang.SecurityException: private phone output", ""},
		{"Status: ok", "Exception: private phone output"},
		{"Status: ok", "Error: private phone output"},
		{"Status: ok\nStatus: timeout", ""},
	} {
		for _, cmd := range []Command{{Op: OpOpenApp, Args: map[string]any{"app": "maps"}}, {Op: OpDial, Args: map[string]any{"number": "123"}}} {
			t.Run(string(cmd.Op)+"/"+output.stdout+output.stderr, func(t *testing.T) {
				fakeAndroidADB(t)
				t.Setenv("ANDROID_TEST_OUTPUT", output.stdout)
				t.Setenv("ANDROID_TEST_STDERR", output.stderr)
				state, err := NewAndroid(androidTestConfig()).Apply(context.Background(), cmd)
				if err == nil || state != nil {
					t.Fatalf("misleading am success accepted: %v, %v", state, err)
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatalf("private activity output leaked: %v", err)
				}
			})
		}
	}
}

func TestAndroidMissingBinaryAndCancelledContext(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		state, err := NewAndroid(androidTestConfig()).Read(context.Background())
		if err == nil || state != nil || !strings.Contains(err.Error(), "adb installation") {
			t.Fatalf("missing adb = %v, %v", state, err)
		}
	})
	t.Run("cancelled before execution", func(t *testing.T) {
		log := fakeAndroidADB(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		state, err := NewAndroid(androidTestConfig()).Apply(ctx, Command{Op: OpPause})
		if !errors.Is(err, context.Canceled) || state != nil || androidCalls(t, log) != "" {
			t.Fatalf("cancelled request = %v, %v; calls = %q", state, err, androidCalls(t, log))
		}
	})
	t.Run("short caller deadline", func(t *testing.T) {
		fakeAndroidADB(t)
		t.Setenv("ANDROID_TEST_ACTION_DELAY", "5")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		state, err := NewAndroid(androidTestConfig()).Apply(ctx, Command{Op: OpPause})
		if !errors.Is(err, context.DeadlineExceeded) || state != nil || time.Since(start) > 2*time.Second {
			t.Fatalf("caller deadline not respected: %v, %v after %v", state, err, time.Since(start))
		}
	})
}

func TestAndroidTimeoutBoundsWholeOperation(t *testing.T) {
	log := fakeAndroidADB(t)
	// Two six-second commands would succeed with a fresh ten-second deadline
	// for each. They must instead exhaust the shared ten-second operation limit.
	t.Setenv("ANDROID_TEST_CHECK_DELAY", "6")
	t.Setenv("ANDROID_TEST_ACTION_DELAY", "6")
	start := time.Now()
	state, err := NewAndroid(androidTestConfig()).Apply(context.Background(), Command{Op: OpPause})
	elapsed := time.Since(start)
	if androidTimeout != 10*time.Second || !errors.Is(err, context.DeadlineExceeded) || state != nil {
		t.Fatalf("whole-operation timeout = %v, %v after %v", state, err, elapsed)
	}
	if elapsed < 9*time.Second || elapsed > 11*time.Second {
		t.Fatalf("ten-second timeout took %v", elapsed)
	}
	if want := androidWantCall("get-state") + androidWantCall("shell", "input", "keyevent", "127"); androidCalls(t, log) != want {
		t.Fatalf("expected check and mutation, got %q", androidCalls(t, log))
	}
}

func TestAndroidRegistryAndImmutableAllowlists(t *testing.T) {
	log := fakeAndroidADB(t)
	cfg := androidTestConfig()
	r := Build(config.Config{Android: &cfg})
	dev, err := r.Get("PHONE")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Kind() != "phone" || len(r.List()) != 1 {
		t.Fatalf("incorrect registry device: %v", r.Infos())
	}
	if description := dev.Describe(); strings.Contains(description, cfg.Serial) || strings.Contains(description, "com.example") {
		t.Fatalf("incorrect public description: %q", description)
	}
	cfg.Apps["maps"] = "com.evil.app"
	cfg.Operations[0] = "shell"
	caps := dev.Capabilities()
	caps[0] = Op("shell")
	if !Supports(dev, OpRead) || Supports(dev, Op("shell")) {
		t.Fatal("caller mutated driver capabilities")
	}
	if _, err := dev.Apply(context.Background(), Command{Op: OpOpenApp, Args: map[string]any{"app": "maps"}}); err != nil {
		t.Fatal(err)
	}
	if calls := androidCalls(t, log); !strings.Contains(calls, "com.example.maps\n") || strings.Contains(calls, "com.evil.app") {
		t.Fatalf("caller mutated app allowlist: %q", calls)
	}
	if len(Build(config.Config{}).List()) != 0 {
		t.Fatal("Android registered when disabled")
	}
}
