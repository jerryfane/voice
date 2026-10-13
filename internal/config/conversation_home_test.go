package config

import (
	"strings"
	"testing"
	"time"
)

func TestConversationRequiresBoundedPairedLocalWakeSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		window   time.Duration
		turns    int
		detector string
		valid    bool
	}{
		{"disabled", 0, 0, "stt", true},
		{"enabled", 20 * time.Second, 2, "sherpa", true},
		{"maximum", 2 * time.Minute, 8, "sherpa", true},
		{"window only", time.Second, 0, "sherpa", false},
		{"turns only", 0, 1, "sherpa", false},
		{"remote gate", time.Second, 1, "stt", false},
		{"unbounded time", 2*time.Minute + time.Nanosecond, 1, "sherpa", false},
		{"unbounded turns", time.Second, 9, "sherpa", false},
		{"negative time", -time.Second, 0, "sherpa", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Wake.Detector = tc.detector
			c.Wake.ConversationWindow = Duration(tc.window)
			c.Wake.ConversationTurns = tc.turns
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("validation=%v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestHomeAssistantRejectsUnsafeAndAmbiguousConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*HomeAssistant)
	}{
		{"embedded token", func(h *HomeAssistant) { h.URL = "http://token@localhost:8123" }},
		{"query token", func(h *HomeAssistant) { h.URL += "?token=secret" }},
		{"not HTTP", func(h *HomeAssistant) { h.URL = "file:///etc/passwd" }},
		{"token instead of variable", func(h *HomeAssistant) { h.TokenEnv = "secret.value" }},
		{"entity path", func(h *HomeAssistant) { h.Entities[0].EntityID = "light.room/../lock.door" }},
		{"unknown domain", func(h *HomeAssistant) { h.Entities[0].EntityID = "script.unlock" }},
		{"unlock", func(h *HomeAssistant) {
			h.Entities[0].EntityID = "lock.door"
			h.Entities[0].Operations = []string{"unlock"}
		}},
		{"cross domain operation", func(h *HomeAssistant) { h.Entities[0].Operations = []string{"temperature"} }},
		{"duplicate operation", func(h *HomeAssistant) { h.Entities[0].Operations = []string{"on", "on"} }},
		{"duplicate ID", func(h *HomeAssistant) {
			h.Entities = append(h.Entities, HomeAssistantEntity{ID: "ROOM", EntityID: "light.second"})
		}},
		{"duplicate entity", func(h *HomeAssistant) {
			h.Entities = append(h.Entities, HomeAssistantEntity{ID: "second", EntityID: "light.room"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.HomeAssistant = &HomeAssistant{URL: "http://localhost:8123", Entities: []HomeAssistantEntity{{ID: "room", EntityID: "light.room", Operations: []string{"on"}}}}
			tc.mutate(c.HomeAssistant)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "home_assistant") && !strings.Contains(err.Error(), "duplicate device") {
				t.Fatalf("unsafe config accepted: %v", err)
			}
		})
	}
}

func TestHomeAssistantRejectsExistingDeviceAliasCollision(t *testing.T) {
	c := Default()
	c.Lights = []Light{{ID: "room", Host: "192.0.2.1"}}
	c.HomeAssistant = &HomeAssistant{URL: "http://localhost:8123", Entities: []HomeAssistantEntity{{ID: "ROOM", EntityID: "light.other"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate device") {
		t.Fatalf("ambiguous device identity accepted: %v", err)
	}
}
