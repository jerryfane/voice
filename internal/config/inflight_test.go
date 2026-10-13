package config

import (
	"strings"
	"testing"
	"time"
)

func TestInFlightWindowRequiresBoundedLocalWakeGate(t *testing.T) {
	c := Default()
	c.Wake.InFlightTurns = 2
	c.Wake.InFlightWindow = Duration(6 * time.Second)
	if err := c.Validate(); err != nil {
		t.Fatalf("bounded Sherpa window rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"cloud-gated", func(c *Config) { c.Wake.Detector = "stt"; c.Wake.Sherpa = nil }, "local sherpa"},
		{"unbounded", func(c *Config) { c.Wake.InFlightWindow = 0 }, "positive"},
		{"too-long", func(c *Config) { c.Wake.InFlightWindow = Duration(time.Minute) }, "30s"},
		{"too-many-turns", func(c *Config) { c.Wake.InFlightTurns = 4 }, "between 0 and 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := c
			tc.edit(&bad)
			if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe in-flight configuration accepted: %v", err)
			}
		})
	}
}
