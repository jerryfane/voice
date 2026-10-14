package brain

import (
	"strings"

	"github.com/jerryfane/voice/internal/device"
)

// phoneQuickCommand recognises complete imperative forms only. The target is
// resolved separately so an unknown or ambiguous name goes to the planner.
func phoneQuickCommand(text string) (device.Op, string, bool) {
	text = strings.TrimSuffix(strings.TrimPrefix(text, "please "), " please")
	for _, prefix := range []string{"wake up ", "wake "} {
		if target, ok := strings.CutPrefix(text, prefix); ok && target != "" {
			return device.OpWake, target, true
		}
	}
	for _, command := range []struct {
		text string
		op   device.Op
	}{
		{"go home", device.OpHome},
		{"go to home", device.OpHome},
		{"go to the home screen", device.OpHome},
		{"go to home screen", device.OpHome},
		{"open settings", device.OpSettings},
		{"open android settings", device.OpSettings},
	} {
		if text == command.text {
			return command.op, "", true
		}
		if target, ok := strings.CutPrefix(text, command.text+" on "); ok && target != "" {
			return command.op, target, true
		}
	}
	return "", "", false
}

func phoneQuickTarget(text, target string, op device.Op, inv []device.Info) (device.Info, bool) {
	generic := target == "" || target == "phone" || target == "the phone"
	selected := -1
	for i, d := range inv {
		if d.Kind != "phone" {
			continue
		}
		id := normalise(d.ID)
		if id == "" || (!generic && target != id && target != "the "+id) {
			continue
		}
		// Count phones before capabilities: a disabled phone is not permission
		// to silently redirect a generic request to a different phone.
		if selected != -1 {
			return device.Info{}, false
		}
		selected = i
	}
	if selected == -1 {
		return device.Info{}, false
	}
	// Preserve named-device collision protection, including non-phone devices
	// named "phone" and IDs that overlap after punctuation normalisation.
	for i, d := range inv {
		id := normalise(d.ID)
		if i != selected && id != "" && strings.Contains(" "+text+" ", " "+id+" ") {
			return device.Info{}, false
		}
	}
	for _, capability := range inv[selected].Capabilities {
		if capability == string(op) {
			return inv[selected], true
		}
	}
	return device.Info{}, false
}
