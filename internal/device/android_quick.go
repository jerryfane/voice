package device

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var (
	androidComponentName = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+)/([.]?[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)$`)
	androidFocusWindow   = regexp.MustCompile(`^Window\{[0-9a-fA-F]+ u[0-9]+ ([^ ]+)\}$`)
)

func androidQuickOp(op Op) bool {
	return op == OpWake || op == OpHome || op == OpSettings
}

// applyQuick uses the same pinned, sanitized ADB transport as legacy actions.
// One deadline covers resolution, mutation and a single read-back phase. Native
// window policy and power evidence avoid UI automation, sleeps and polling.
func (d *Android) applyQuick(ctx context.Context, op Op) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, androidTimeout)
	defer cancel()

	var target string
	if op != OpWake {
		args := []string{"shell", "cmd", "package", "resolve-activity", "--components", "--user", "current", "-a"}
		if op == OpHome {
			args = append(args, "android.intent.action.MAIN", "-c", "android.intent.category.HOME")
		} else {
			args = append(args, "android.settings.SETTINGS")
		}
		out, err := d.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		target = androidComponent(strings.TrimSpace(string(out.Stdout)))
		if target == "" || len(out.Stderr) != 0 {
			return nil, errors.New("Android could not resolve a unique target activity; check the phone")
		}
	}
	if _, err := d.run(ctx, "shell", "input", "keyevent", "KEYCODE_WAKEUP"); err != nil {
		return nil, err
	}
	switch op {
	case OpHome:
		if _, err := d.run(ctx, "shell", "input", "keyevent", "KEYCODE_HOME"); err != nil {
			return nil, err
		}
	case OpSettings:
		// A component may contain '$' in an inner class name. The validated
		// grammar excludes quotes; quote it for ADB's remote shell literally.
		out, err := d.run(ctx, "shell", "am", "start", "-W", "--user", "current", "-a", "android.settings.SETTINGS", "-n", "'"+target+"'")
		if err != nil {
			return nil, err
		}
		if !androidActivityAccepted(out) {
			return nil, errors.New("Android did not acknowledge the Settings request; check the phone")
		}
	}

	power, err := d.run(ctx, "shell", "dumpsys", "power")
	if err != nil {
		return nil, err
	}
	window, err := d.run(ctx, "shell", "dumpsys", "window")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	windowText := string(window.Stdout)
	if len(power.Stderr) != 0 || len(window.Stderr) != 0 || !androidAwake(string(power.Stdout), windowText) {
		return nil, errors.New("Android awake display could not be verified; check the phone")
	}
	if op == OpWake {
		// Waking a locked screen is valid; do not claim that it is unlocked.
		return State{"receipt": "Screen is awake."}, nil
	}
	if !androidUnlocked(windowText) {
		return nil, errors.New("Android unlocked screen could not be verified; unlock the phone manually")
	}
	if androidForeground(windowText) != target {
		return nil, errors.New("Android requested foreground could not be verified; check the phone")
	}
	if op == OpHome {
		return State{"receipt": "Home screen is visible."}, nil
	}
	return State{"receipt": "Settings is open."}, nil
}

// Accept one exact app component, not a package match, resolver UI, diagnostic,
// multiple candidates or shell text. Normalize Android's short class notation.
func androidComponent(value string) string {
	if !androidComponentName.MatchString(value) {
		return ""
	}
	pkg, class, _ := strings.Cut(value, "/")
	if strings.HasPrefix(class, ".") {
		class = pkg + class
	}
	if strings.HasSuffix(class, ".ResolverActivity") || strings.HasSuffix(class, ".ChooserActivity") {
		return ""
	}
	if value[len(pkg)+1] != '.' {
		return value
	}
	return pkg + "/" + class
}

func androidAwake(power, window string) bool {
	// The global field is indented two spaces. Per-power-group dumps can also
	// contain mWakefulness=1; those are not global display evidence.
	awake := false
	for line := range strings.SplitSeq(power, "\n") {
		if strings.HasPrefix(line, "  mWakefulness=") {
			if awake || strings.TrimSpace(line) != "mWakefulness=Awake" {
				return false
			}
			awake = true
		}
	}
	// Wakefulness alone does not imply the display has finished turning on.
	// Window policy supplies actual completed screen-on evidence on current
	// Android, whose power dump no longer includes Display Power: state=ON.
	return awake && androidWindowFlag(window, "mAwake", "true") &&
		androidWindowFlag(window, "mScreenOnEarly", "true") &&
		androidWindowFlag(window, "mScreenOnFully", "true")
}

// Window policy fields are whitespace-delimited key=value tokens. Require one
// unambiguous value: missing, malformed and duplicated evidence fails closed.
func androidWindowFlag(window, key, value string) bool {
	seen := false
	prefix, expected := key+"=", key+"="+value
	for field := range strings.FieldsSeq(window) {
		if strings.HasPrefix(field, prefix) {
			if seen || field != expected {
				return false
			}
			seen = true
		}
	}
	return seen
}

func androidUnlocked(window string) bool {
	return androidWindowFlag(window, "isKeyguardShowing", "false") &&
		androidWindowFlag(window, "mShowingDream", "false") &&
		androidWindowFlag(window, "mDreamingLockscreen", "false") &&
		androidKeyguardUnrestricted(window)
}

// Scope these generic field names to the native KeyguardServiceDelegate
// section; other services also print showing/inputRestricted properties.
func androidKeyguardUnrestricted(window string) bool {
	section, showing, restricted := false, false, false
	depth := -1
	for line := range strings.SplitSeq(window, "\n") {
		text := strings.TrimLeft(line, " ")
		indent := len(line) - len(text)
		if text == "KeyguardServiceDelegate" {
			if section {
				return false
			}
			section, depth = true, indent
			continue
		}
		if depth < 0 || text == "" {
			continue
		}
		if indent <= depth {
			depth = -1
			continue
		}
		if strings.HasPrefix(text, "showing=") {
			if showing || text != "showing=false" {
				return false
			}
			showing = true
		}
		if strings.HasPrefix(text, "inputRestricted=") {
			if restricted || text != "inputRestricted=false" {
				return false
			}
			restricted = true
		}
	}
	return section && showing && restricted
}

func androidForeground(window string) string {
	var component string
	seen := false
	for line := range strings.SplitSeq(window, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "mCurrentFocus=") {
			continue
		}
		if seen {
			return ""
		}
		seen = true
		value := strings.TrimPrefix(line, "mCurrentFocus=")
		if !androidFocusWindow.MatchString(value) {
			return ""
		}
		// The validated window format ends in the exact activity component.
		component = androidComponent(value[strings.LastIndexByte(value, ' ')+1 : len(value)-1])
	}
	return component
}
