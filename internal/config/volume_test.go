package config

import (
	"testing"

	"github.com/jerryfane/voice/internal/audio"
)

// The thinking sound's level must be settable on its own, and an existing
// config that never mentions it must keep the level it had. Both halves
// matter: the owner asked the device for this knob and was told it could not
// be adjusted, and anyone who had tuned feedback.volume must not find the
// thinking sound suddenly louder.
func TestThinkingVolumeIsSettableAndDefaultsToAShareOfVolume(t *testing.T) {
	// Unset: derived from Volume, which is the behaviour before this field.
	f := Feedback{Volume: 0.6}
	if got, want := f.ResolvedThinkingVolume(), 0.6*audio.ThinkingVolumeRatio; got != want {
		t.Errorf("unset thinking volume = %v, want %v (a share of Volume)", got, want)
	}

	// Set: used as given, independent of Volume.
	f = Feedback{Volume: 0.2, ThinkingVolume: 0.9}
	if got := f.ResolvedThinkingVolume(); got != 0.9 {
		t.Errorf("explicit thinking volume = %v, want 0.9 regardless of Volume", got)
	}

	// Louder than the acknowledgement is allowed: it is the user's room.
	f = Feedback{Volume: 0.1, ThinkingVolume: 1}
	if got := f.ResolvedThinkingVolume(); got <= f.Volume {
		t.Errorf("an explicit %v must not be clamped down to Volume %v", got, f.Volume)
	}
}

// A value outside 0..1 must be rejected at startup, like feedback.volume,
// rather than silently clamped where nobody sees it.
func TestThinkingVolumeIsValidated(t *testing.T) {
	for _, v := range []float64{-0.1, 1.5} {
		c := Default()
		c.Feedback.ThinkingVolume = v
		if err := c.Validate(); err == nil {
			t.Errorf("thinking_volume %v was accepted; it must be rejected with a message", v)
		}
	}
	c := Default()
	c.Feedback.ThinkingVolume = 0.5
	if err := c.Validate(); err != nil {
		t.Errorf("a valid thinking_volume was rejected: %v", err)
	}
}

// The shipped default must not change what anyone hears today.
func TestShippedDefaultKeepsTheExistingThinkingLevel(t *testing.T) {
	c := Default()
	if c.Feedback.ThinkingVolume != 0 {
		t.Errorf("thinking_volume defaults to %v; it must default to unset so existing configs are unchanged", c.Feedback.ThinkingVolume)
	}
	if got, want := c.Feedback.ResolvedThinkingVolume(), c.Feedback.Volume*audio.ThinkingVolumeRatio; got != want {
		t.Errorf("default resolves to %v, want %v", got, want)
	}
}
