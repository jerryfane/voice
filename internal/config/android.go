package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Android opts into controlling one already trusted, connected ADB target.
// Operations grants no permissions when omitted. Apps maps owner-chosen aliases
// to launcher packages; neither arbitrary intents nor shell commands are exposed.
// Installing adb and authorizing its host key on the phone are owner tasks.
type Android struct {
	ID         string            `json:"id"`
	Serial     string            `json:"serial"`
	Operations []string          `json:"operations,omitempty"`
	Apps       map[string]string `json:"apps,omitempty"`
}

var (
	androidSerial  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}$`)
	androidAlias   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	androidPackage = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
)

// Validate is also used by the device constructor, which may be called without
// Config.Validate. Never include serials or rejected values in these errors.
func (a Android) Validate() error {
	if a.ID == "" || strings.TrimSpace(a.ID) != a.ID {
		return fmt.Errorf("android.id must be nonempty without surrounding whitespace")
	}
	if !androidSerial.MatchString(a.Serial) {
		return fmt.Errorf("android.serial must be an explicit ADB serial using only ASCII letters, digits, dot, underscore, colon, or hyphen, starting with a letter or digit")
	}
	seen := make(map[string]bool)
	for _, op := range a.Operations {
		switch op {
		case "read", "open_app", "dial", "volume_up", "volume_down", "pause", "resume":
		default:
			return fmt.Errorf("android.operations contains an unsupported operation")
		}
		if seen[op] {
			return fmt.Errorf("android.operations contains a repeated operation")
		}
		seen[op] = true
	}
	for alias, pkg := range a.Apps {
		if !androidAlias.MatchString(alias) {
			return fmt.Errorf("android.apps aliases must use ASCII letters, digits, underscore, or hyphen, starting with a letter or digit (maximum 64 characters)")
		}
		if len(pkg) > 255 || !androidPackage.MatchString(pkg) {
			return fmt.Errorf("android.apps must contain dotted Android package names using only ASCII letters, digits, and underscores (maximum 255 characters)")
		}
	}
	return nil
}
