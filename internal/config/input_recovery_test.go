package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInputRecoveryIsOptInAndResolvesDefaults(t *testing.T) {
	c := Default()
	if c.Input.RecoveryEnabled() {
		t.Fatal("legacy config enabled recovery")
	}
	if err := json.Unmarshal([]byte(`{"recovery":{},"fallbacks":[{"device":"backup","command":["capture","{device}","{rate}"],"output":{"device":"speaker","command":["play","{device}"]}}]}`), &c.Input); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Input.RecoveryEnabled() {
		t.Fatal("configured recovery disabled")
	}
	c.Input.Recovery = nil
	if !c.Input.RecoveryEnabled() {
		t.Fatal("fallback alone should enable recovery")
	}
}

func TestInputRecoveryDurationsAreBounded(t *testing.T) {
	for _, test := range []struct {
		name     string
		set      func(*InputRecovery, Duration)
		min, max time.Duration
	}{
		{"ready_timeout", func(r *InputRecovery, d Duration) { r.ReadyTimeout = d }, 100 * time.Millisecond, time.Minute},
		{"stall_timeout", func(r *InputRecovery, d Duration) { r.StallTimeout = d }, 100 * time.Millisecond, time.Minute},
		{"zero_timeout", func(r *InputRecovery, d Duration) { r.ZeroTimeout = d }, time.Second, 10 * time.Minute},
		{"retry_interval", func(r *InputRecovery, d Duration) { r.RetryInterval = d }, 100 * time.Millisecond, time.Minute},
		{"preferred_interval", func(r *InputRecovery, d Duration) { r.PreferredInterval = d }, time.Second, time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range []time.Duration{-1, test.min - time.Nanosecond, test.max + time.Nanosecond} {
				c := Default()
				c.Input.Recovery = &InputRecovery{}
				test.set(c.Input.Recovery, Duration(value))
				if err := c.Validate(); err == nil || !strings.Contains(err.Error(), test.name) {
					t.Fatalf("%s=%s error=%v", test.name, value, err)
				}
			}
			for _, value := range []time.Duration{test.min, test.max} {
				c := Default()
				c.Input.Recovery = &InputRecovery{}
				test.set(c.Input.Recovery, Duration(value))
				if err := c.Validate(); err != nil {
					t.Fatalf("valid boundary %s: %v", value, err)
				}
			}
		})
	}
}

func TestInputFallbacksRequireCaptureAndValidPairedPlayback(t *testing.T) {
	for _, fallback := range []InputFallback{{}, {Command: Command{" "}}, {Command: Command{"capture"}, Output: &Output{}}, {Command: Command{"capture"}, Output: &Output{Command: Command{" "}}}} {
		c := Default()
		c.Input.Fallbacks = []InputFallback{fallback}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "input.fallbacks[0]") {
			t.Fatalf("invalid fallback error=%v", err)
		}
	}
	c := Default()
	c.Input.Fallbacks = make([]InputFallback, 9)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "at most 8") {
		t.Fatalf("unbounded choices error=%v", err)
	}
}

func TestRecoveryRejectsUnboundedUtteranceBoundary(t *testing.T) {
	c := Default()
	c.Input.Recovery = &InputRecovery{}
	c.Wake.VAD.Silence = Duration(time.Hour + time.Nanosecond)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "wake.vad.silence") {
		t.Fatalf("unbounded return boundary accepted: %v", err)
	}
	c.Wake.VAD.Silence = Duration(time.Hour)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
