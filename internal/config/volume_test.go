package config

import (
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/feedback"
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
	nine := 0.9
	f = Feedback{Volume: 0.2, ThinkingVolume: &nine}
	if got := f.ResolvedThinkingVolume(); got != 0.9 {
		t.Errorf("explicit thinking volume = %v, want 0.9 regardless of Volume", got)
	}

	// Louder than the acknowledgement is allowed: it is the user's room.
	full := 1.0
	f = Feedback{Volume: 0.1, ThinkingVolume: &full}
	if got := f.ResolvedThinkingVolume(); got <= f.Volume {
		t.Errorf("an explicit %v must not be clamped down to Volume %v", got, f.Volume)
	}
}

// A value outside 0..1 must be rejected at startup, like feedback.volume,
// rather than silently clamped where nobody sees it.
func TestThinkingVolumeIsValidated(t *testing.T) {
	for _, v := range []float64{-0.1, 1.5} {
		c := Default()
		c.Feedback.ThinkingVolume = &v
		if err := c.Validate(); err == nil {
			t.Errorf("thinking_volume %v was accepted; it must be rejected with a message", v)
		}
	}
	c := Default()
	half := 0.5
	c.Feedback.ThinkingVolume = &half
	if err := c.Validate(); err != nil {
		t.Errorf("a valid thinking_volume was rejected: %v", err)
	}
}

// The shipped default must not change what anyone hears today.
func TestShippedDefaultKeepsTheExistingThinkingLevel(t *testing.T) {
	c := Default()
	if c.Feedback.ThinkingVolume != nil {
		t.Errorf("thinking_volume defaults to %v; it must default to UNSET so existing configs are unchanged", *c.Feedback.ThinkingVolume)
	}
	if got, want := c.Feedback.ResolvedThinkingVolume(), c.Feedback.Volume*audio.ThinkingVolumeRatio; got != want {
		t.Errorf("default resolves to %v, want %v", got, want)
	}
}

// Zero must mean SILENT, not "unset". With a plain float64 the two are
// indistinguishable, so the one value a user most obviously wants from a
// volume setting - turn it off - would have been read as "derive from Volume"
// and ignored. Caught in review before it shipped.
func TestAnExplicitZeroMutesTheThinkingSound(t *testing.T) {
	zero := 0.0
	f := Feedback{Volume: 0.9, ThinkingVolume: &zero}
	if got := f.ResolvedThinkingVolume(); got != 0 {
		t.Fatalf("explicit thinking_volume 0 resolved to %v; it must mute", got)
	}

	// And silence must survive all the way to the rendered audio, not just
	// the resolver: a level of zero that still produces samples is not mute.
	pcm, err := audio.Thinking(audio.ThinkingTick, audio.Format{SampleRate: 16000, Channels: 1}, f.ResolvedThinkingVolume())
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) != 0 {
		t.Errorf("rendered %d samples at volume 0; the thinking sound must be silent", len(pcm))
	}

	// A config asking for silence must still validate: it is a legitimate
	// choice, not an error.
	c := Default()
	c.Feedback.ThinkingVolume = &zero
	if err := c.Validate(); err != nil {
		t.Errorf("thinking_volume 0 was rejected: %v", err)
	}
}

func TestShippedWakeFeedbackIsAudibleAndFollowUpIsBounded(t *testing.T) {
	c := Default()
	if c.Feedback.Volume != 0.65 {
		t.Fatalf("feedback volume = %v, want owner-selected 0.65", c.Feedback.Volume)
	}
	if c.Wake.FollowUpTimeout.D() != 6*time.Second {
		t.Fatalf("follow-up timeout = %s, want 6s", c.Wake.FollowUpTimeout.D())
	}
	c.Wake.FollowUpTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatal("zero follow-up timeout was accepted")
	}
}

func TestThinkingDelayDefaultsToPromptFeedbackAndMustBePositive(t *testing.T) {
	c := Default()
	if got := c.Feedback.ThinkingDelay.D(); got != feedback.DefaultThinkingDelay {
		t.Fatalf("thinking delay = %s, want %s", got, feedback.DefaultThinkingDelay)
	}
	c.Feedback.ThinkingDelay = 0
	if err := c.Validate(); err == nil {
		t.Fatal("zero thinking delay was accepted")
	}
}
