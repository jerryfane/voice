package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

const quickPower = "POWER MANAGER (dumpsys power)\n  mWakefulness=Awake\nDisplay Power: com.android.server.power.PowerManagerService$3@123\n    mWakefulness=1\n"
const quickWindow = "WINDOW MANAGER WINDOWS (dumpsys window windows)\n  mCurrentFocus=Window{49d0912 u0 com.vendor.settings/com.vendor.settings.HomeActivity}\nWINDOW MANAGER POLICY STATE (dumpsys window policy)\n    mAwake=true mScreenOnEarly=true mScreenOnFully=true\n    mShowingDream=false mDreamingLockscreen=false\n    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n      secure=true\n    isKeyguardShowing=false\n"

func quickAndroidConfig() config.Android {
	return config.Android{ID: "phone", Serial: "PINNED-PHONE", AgentControl: true}
}

// Unlike a command-echo mock, this transport supplies independent resolver,
// activity acknowledgement, power and foreground evidence. Acknowledgement is
// deliberately always successful, even when the observed outcome is wrong.
func fakeQuickAndroidADB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	body := `#!/bin/sh
printf 'BEGIN\n' >> "$ANDROID_TEST_LOG"
printf '%s\n' "$@" >> "$ANDROID_TEST_LOG"
if [ "$1" != '-s' ] || [ "$2" != "$QUICK_SERIAL" ]; then exit 90; fi
shift 2
case "$*" in
  'shell cmd package resolve-activity --components --user current -a android.intent.action.MAIN -c android.intent.category.HOME') phase=resolve; output="$QUICK_HOME" ;;
  'shell cmd package resolve-activity --components --user current -a android.settings.SETTINGS') phase=resolve; output="$QUICK_SETTINGS" ;;
  'shell input keyevent KEYCODE_WAKEUP') phase=wake; output='' ;;
  'shell input keyevent KEYCODE_HOME') phase=home; output='' ;;
  "shell am start -W --user current -a android.settings.SETTINGS -n 'com.vendor.settings/com.vendor.settings.HomeActivity'"|"shell am start -W --user current -a android.settings.SETTINGS -n 'com.vendor.settings/com.vendor.settings.Settings\$Home'") phase=settings; output='Status: ok' ;;
  'shell dumpsys power') phase=power; output="$QUICK_POWER" ;;
  'shell dumpsys window') phase=window; output="$QUICK_WINDOW" ;;
  *) printf 'private unexpected command' >&2; exit 91 ;;
esac
if [ "$QUICK_DELAY" = "$phase" ]; then exec /bin/sleep 5; fi
if [ "$QUICK_FAIL" = "$phase" ]; then printf 'PINNED-PHONE private phone data' >&2; exit 1; fi
printf '%s\n' "$output"
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ANDROID_TEST_LOG", log)
	t.Setenv("QUICK_SERIAL", "PINNED-PHONE")
	t.Setenv("QUICK_HOME", "com.vendor.launcher/.Launcher")
	t.Setenv("QUICK_SETTINGS", "com.vendor.settings/.HomeActivity")
	t.Setenv("QUICK_POWER", quickPower)
	t.Setenv("QUICK_WINDOW", quickWindow)
	t.Setenv("QUICK_FAIL", "")
	t.Setenv("QUICK_DELAY", "")
	return log
}

func TestAndroidQuickVerifiedOutcomes(t *testing.T) {
	for _, op := range []Op{OpWake, OpHome, OpSettings} {
		t.Run(string(op), func(t *testing.T) {
			log := fakeQuickAndroidADB(t)
			if op == OpHome {
				t.Setenv("QUICK_WINDOW", strings.ReplaceAll(quickWindow, "com.vendor.settings/com.vendor.settings.HomeActivity", "com.vendor.launcher/.Launcher"))
			}
			state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: op})
			if err != nil || len(state) != 1 || state["receipt"] == "" {
				t.Fatalf("verified outcome = %v, %v", state, err)
			}
			calls := androidCalls(t, log)
			if strings.Count(calls, "\ndumpsys\npower\n") != 1 || strings.Count(calls, "\ndumpsys\nwindow\n") != 1 {
				t.Fatalf("expected one native verification phase, got %q", calls)
			}
			if strings.Contains(calls, "uiautomator") || strings.Contains(calls, "screencap") || strings.Contains(calls, "dismiss-keyguard") {
				t.Fatalf("fast path attempted unrelated or authentication operation: %q", calls)
			}
		})
	}
}

func TestAndroidQuickRejectsUnverifiedOutcomes(t *testing.T) {
	tests := []struct {
		name, power, window string
	}{
		{"wrong app", quickPower, strings.ReplaceAll(quickWindow, "com.vendor.settings/com.vendor.settings.HomeActivity", "com.other.app/.Main")},
		{"wrong activity same package", quickPower, strings.ReplaceAll(quickWindow, "settings.HomeActivity", "settings.OtherActivity")},
		{"missing foreground", quickPower, strings.ReplaceAll(quickWindow, "mCurrentFocus=", "previousFocus=")},
		{"null foreground", quickPower, strings.ReplaceAll(quickWindow, "Window{49d0912 u0 com.vendor.settings/com.vendor.settings.HomeActivity}", "null")},
		{"non app foreground", quickPower, strings.ReplaceAll(quickWindow, "com.vendor.settings/com.vendor.settings.HomeActivity", "NotificationShade")},
		{"multiple foregrounds", quickPower, quickWindow + "  mCurrentFocus=Window{123 u0 com.vendor.settings/.HomeActivity}\n"},
		{"locked", quickPower, strings.ReplaceAll(quickWindow, "isKeyguardShowing=false", "isKeyguardShowing=true")},
		{"missing lock evidence", quickPower, strings.ReplaceAll(quickWindow, "isKeyguardShowing=false", "")},
		{"dreaming lockscreen", quickPower, strings.ReplaceAll(quickWindow, "mDreamingLockscreen=false", "mDreamingLockscreen=true")},
		{"keyguard delegate locked", quickPower, strings.ReplaceAll(quickWindow, "showing=false", "showing=true")},
		{"input restricted", quickPower, strings.ReplaceAll(quickWindow, "inputRestricted=false", "inputRestricted=true")},
		{"absent keyguard delegate", quickPower, strings.ReplaceAll(quickWindow, "KeyguardServiceDelegate", "OtherService")},
		{"absent input restriction evidence", quickPower, strings.ReplaceAll(quickWindow, "inputRestricted=false", "")},
		{"unscoped input restriction evidence", quickPower, strings.ReplaceAll(quickWindow, "      inputRestricted=false", "  inputRestricted=false")},
		{"duplicate keyguard delegate", quickPower, quickWindow + "    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n"},
		{"asleep", strings.ReplaceAll(quickPower, "=Awake", "=Asleep"), quickWindow},
		{"awake but screen off", quickPower, strings.ReplaceAll(quickWindow, "mScreenOnFully=true", "mScreenOnFully=false")},
		{"screen transition", quickPower, strings.ReplaceAll(quickWindow, "mScreenOnEarly=true", "mScreenOnEarly=false")},
		{"empty power", "", quickWindow},
		{"empty window", quickPower, ""},
		{"numeric group only", "    mWakefulness=1\n", quickWindow},
		{"duplicate global power", quickPower + "  mWakefulness=Awake\n", quickWindow},
		{"malformed screen state", quickPower, strings.ReplaceAll(quickWindow, "mScreenOnFully=true", "mScreenOnFully=trueish")},
		{"conflicting screen state", quickPower, quickWindow + "mScreenOnFully=false\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeQuickAndroidADB(t)
			t.Setenv("QUICK_POWER", tt.power)
			t.Setenv("QUICK_WINDOW", tt.window)
			state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpSettings})
			if err == nil || state != nil {
				t.Fatalf("unverified outcome reported success: %v, %v", state, err)
			}
			if strings.Contains(err.Error(), "com.vendor") || strings.Contains(err.Error(), "49d0912") {
				t.Fatalf("private evidence leaked: %v", err)
			}
		})
	}
}

func TestAndroidQuickHomeRequiresResolvedLauncher(t *testing.T) {
	fakeQuickAndroidADB(t)
	// Even an unlocked, awake Settings foreground is not the resolved HOME.
	state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpHome})
	if err == nil || state != nil {
		t.Fatalf("wrong foreground accepted as Home: %v, %v", state, err)
	}
}

func TestAndroidQuickWakeDoesNotClaimUnlock(t *testing.T) {
	fakeQuickAndroidADB(t)
	t.Setenv("QUICK_WINDOW", strings.ReplaceAll(quickWindow, "isKeyguardShowing=false", "isKeyguardShowing=true"))
	state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpWake})
	if err != nil || state == nil {
		t.Fatalf("awake locked screen was not accepted as awake: %v, %v", state, err)
	}
	t.Setenv("QUICK_WINDOW", strings.ReplaceAll(quickWindow, "mScreenOnFully=true", "mScreenOnFully=false"))
	state, err = NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpWake})
	if err == nil || state != nil {
		t.Fatalf("screen-off wake acknowledgement accepted: %v, %v", state, err)
	}
}

func TestAndroidQuickComponentInnerClassIsLiteral(t *testing.T) {
	log := fakeQuickAndroidADB(t)
	t.Setenv("QUICK_SETTINGS", "com.vendor.settings/.Settings$Home")
	t.Setenv("QUICK_WINDOW", strings.ReplaceAll(quickWindow, "com.vendor.settings.HomeActivity", "com.vendor.settings.Settings$Home"))
	state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpSettings})
	if err != nil || state == nil {
		t.Fatalf("literal inner-class activity not verified: %v, %v", state, err)
	}
	if !strings.Contains(androidCalls(t, log), "\n-n\n'com.vendor.settings/com.vendor.settings.Settings$Home'\n") {
		t.Fatal("inner-class component was not protected from remote shell expansion")
	}
}

func TestAndroidQuickResolutionFailsBeforeMutation(t *testing.T) {
	for _, output := range []string{"", "No activity found", "android/com.android.internal.app.ResolverActivity", "com.vendor/.ChooserActivity", "com.vendor/.Main\ncom.other/.Main", "priority=0\ncom.vendor/.Main", "com.vendor/.Main;id", "com.vendor", "com.vendor/"} {
		t.Run(output, func(t *testing.T) {
			log := fakeQuickAndroidADB(t)
			t.Setenv("QUICK_SETTINGS", output)
			state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: OpSettings})
			if err == nil || state != nil || strings.Contains(androidCalls(t, log), "\ninput\n") {
				t.Fatalf("unresolved target mutated phone: %v, %v; %q", state, err, androidCalls(t, log))
			}
		})
	}
}

func TestAndroidQuickPermissionIsolation(t *testing.T) {
	log := fakeQuickAndroidADB(t)
	cfg := quickAndroidConfig()
	d := NewAndroid(cfg)
	for _, op := range []Op{OpWake, OpHome, OpSettings} {
		if !Supports(d, op) {
			t.Fatalf("agent opt-in did not grant %s", op)
		}
		if state, err := d.Apply(context.Background(), Command{Op: op, Args: map[string]any{"serial": "other"}}); err == nil || state != nil {
			t.Fatalf("accepted extra arguments: %v, %v", state, err)
		}
	}
	for _, op := range []Op{OpRead, OpOpenApp, OpDial, OpPause, OpOn, Op("unknown")} {
		if Supports(d, op) {
			t.Fatalf("agent opt-in granted unrelated structured operation %s", op)
		}
		if state, err := d.Apply(context.Background(), Command{Op: op}); err == nil || state != nil {
			t.Fatalf("accepted unconfigured operation %s: %v, %v", op, state, err)
		}
	}
	for _, change := range []func(*config.Android){
		func(c *config.Android) { c.AgentControl = false },
		func(c *config.Android) { c.Serial = "" },
		func(c *config.Android) { c.Operations = []string{"wake"} },
	} {
		cfg := quickAndroidConfig()
		change(&cfg)
		d := NewAndroid(cfg)
		for _, op := range []Op{OpWake, OpHome, OpSettings} {
			if Supports(d, op) {
				t.Fatalf("invalid or disabled config granted %s", op)
			}
			if state, err := d.Apply(context.Background(), Command{Op: op}); err == nil || state != nil {
				t.Fatalf("invalid or disabled config executed %s: %v, %v", op, state, err)
			}
		}
	}
	if calls := androidCalls(t, log); calls != "" {
		t.Fatalf("denied command ran a process: %q", calls)
	}
}

func TestAndroidQuickFixedLiteralSerial(t *testing.T) {
	log := fakeQuickAndroidADB(t)
	cfg := quickAndroidConfig()
	cfg.Serial = "192.0.2.10:5555"
	t.Setenv("QUICK_SERIAL", cfg.Serial)
	if _, err := NewAndroid(cfg).Apply(context.Background(), Command{Op: OpSettings}); err != nil {
		t.Fatal(err)
	}
	for _, call := range strings.Split(androidCalls(t, log), "BEGIN\n")[1:] {
		if !strings.HasPrefix(call, "-s\n"+cfg.Serial+"\n") {
			t.Fatalf("lost literal serial pin: %q", call)
		}
	}
}

func TestAndroidQuickPreservesCancellationAndSanitizesADBErrors(t *testing.T) {
	for _, phase := range []string{"resolve", "wake", "home", "settings", "power", "window"} {
		t.Run("failure/"+phase, func(t *testing.T) {
			fakeQuickAndroidADB(t)
			t.Setenv("QUICK_FAIL", phase)
			op := OpSettings
			if phase == "home" {
				op = OpHome
			}
			state, err := NewAndroid(quickAndroidConfig()).Apply(context.Background(), Command{Op: op})
			if err == nil || state != nil {
				t.Fatalf("ADB failure reported success: %v, %v", state, err)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "PINNED-PHONE") {
				t.Fatalf("private ADB failure leaked: %v", err)
			}
		})
	}
	t.Run("cancelled before execution", func(t *testing.T) {
		log := fakeQuickAndroidADB(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		state, err := NewAndroid(quickAndroidConfig()).Apply(ctx, Command{Op: OpSettings})
		if !errors.Is(err, context.Canceled) || state != nil || androidCalls(t, log) != "" {
			t.Fatalf("ignored caller cancellation: %v, %v", state, err)
		}
	})
	for _, phase := range []string{"resolve", "wake", "power", "window"} {
		t.Run("deadline/"+phase, func(t *testing.T) {
			fakeQuickAndroidADB(t)
			t.Setenv("QUICK_DELAY", phase)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			state, err := NewAndroid(quickAndroidConfig()).Apply(ctx, Command{Op: OpSettings})
			if !errors.Is(err, context.DeadlineExceeded) || state != nil || time.Since(start) > 2*time.Second {
				t.Fatalf("ignored shared caller deadline: %v, %v", state, err)
			}
		})
	}
}
