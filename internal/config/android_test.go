package config

import (
	"strings"
	"testing"
)

func validAndroidConfig() Android {
	return Android{ID: "phone", Serial: "owner-phone_1", Operations: []string{"read", "open_app", "dial", "volume_up", "volume_down", "pause", "resume"}, Apps: map[string]string{"maps": "com.example.maps"}}
}

func TestAndroidConfigDefaultDisabled(t *testing.T) {
	c := Default()
	if c.Android != nil {
		t.Fatal("Android must be disabled by default")
	}
}

func TestAndroidConfigValidation(t *testing.T) {
	for _, serial := range []string{"USB123", "emulator-5554", "192.168.1.2:5555", "adb-ABC._tcp"} {
		a := validAndroidConfig()
		a.Serial = serial
		if err := a.Validate(); err != nil {
			t.Fatalf("valid serial rejected: %v", err)
		}
	}
	for _, ops := range [][]string{nil, {}} {
		a := validAndroidConfig()
		a.Operations = ops
		if err := a.Validate(); err != nil {
			t.Fatalf("empty allowlist is valid: %v", err)
		}
	}
	tests := []struct {
		name string
		edit func(*Android)
	}{
		{"empty ID", func(a *Android) { a.ID = "" }},
		{"padded ID", func(a *Android) { a.ID = " phone " }},
		{"empty serial", func(a *Android) { a.Serial = "" }},
		{"shell serial", func(a *Android) { a.Serial = "serial;id" }},
		{"option serial", func(a *Android) { a.Serial = "-d" }},
		{"space serial", func(a *Android) { a.Serial = "serial extra" }},
		{"newline serial", func(a *Android) { a.Serial = "serial\nid" }},
		{"substitution serial", func(a *Android) { a.Serial = "$(id)" }},
		{"long serial", func(a *Android) { a.Serial = strings.Repeat("s", 256) }},
		{"unsupported op", func(a *Android) { a.Operations = []string{"call"} }},
		{"duplicate op", func(a *Android) { a.Operations = []string{"read", "read"} }},
		{"padded op", func(a *Android) { a.Operations = []string{" read"} }},
		{"shell package", func(a *Android) { a.Apps["maps"] = "com.example.maps;id" }},
		{"component package", func(a *Android) { a.Apps["maps"] = "com.example/.Main" }},
		{"substitution package", func(a *Android) { a.Apps["maps"] = "com.example.$(id)" }},
		{"space package", func(a *Android) { a.Apps["maps"] = "com.example --user all" }},
		{"empty package", func(a *Android) { a.Apps["maps"] = "" }},
		{"long package", func(a *Android) { a.Apps["maps"] = "com." + strings.Repeat("a", 252) }},
		{"shell alias", func(a *Android) { a.Apps["maps;id"] = "com.example.maps" }},
		{"space alias", func(a *Android) { a.Apps["my maps"] = "com.example.maps" }},
		{"empty alias", func(a *Android) { a.Apps[""] = "com.example.maps" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validAndroidConfig()
			tt.edit(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestAndroidDeviceIDMustBeUniqueAcrossKinds(t *testing.T) {
	for _, kind := range []string{"light", "tv", "spotify", "home_assistant"} {
		t.Run(kind, func(t *testing.T) {
			c := Default()
			a := validAndroidConfig()
			c.Android = &a
			switch kind {
			case "light":
				c.Lights = []Light{{ID: "PHONE", Host: "127.0.0.1"}}
			case "tv":
				c.TVs = []TV{{ID: "PHONE", Adapter: "/dev/cec0"}}
			case "spotify":
				c.Spotify = &Spotify{ID: "PHONE", Command: Command{"spotify_player"}}
			case "home_assistant":
				c.HomeAssistant = &HomeAssistant{URL: "http://127.0.0.1:8123", Entities: []HomeAssistantEntity{{ID: "PHONE", EntityID: "light.desk"}}}
			}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate device id") {
				t.Fatalf("duplicate ID error = %v", err)
			}
		})
	}
}
