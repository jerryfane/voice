package config

import (
	"encoding/json"
	"testing"
	"time"

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

// The cloud voice is optional and absent by default: an existing install must
// not start making network calls because it upgraded.
func TestOpenRouterVoiceIsOffByDefault(t *testing.T) {
	if c := Default(); c.TTS.OpenRouter != nil {
		t.Errorf("tts.openrouter defaults to %+v; it must be unset", c.TTS.OpenRouter)
	}
	if got := Default().TTS.Name; got != "piper" {
		t.Errorf("default TTS engine = %q, want the local piper", got)
	}
}

// Configuring the cloud voice must be all-or-nothing. A half-filled block -
// no voice, no key variable - would be accepted and then fail at the moment
// the user speaks, which is the worst time to discover it.
func TestOpenRouterVoiceIsValidatedUpFront(t *testing.T) {
	good := OpenRouterTTS{
		Endpoint:  "https://openrouter.ai/api/v1/chat/completions",
		Model:     "openai/gpt-audio-mini",
		Voice:     "marin",
		APIKeyEnv: "OPENROUTER_API_KEY",
		Timeout:   Duration(30 * time.Second),
	}
	c := Default()
	c.TTS.OpenRouter = &good
	if err := c.Validate(); err != nil {
		t.Fatalf("a complete cloud voice was rejected: %v", err)
	}

	for name, break_ := range map[string]func(*OpenRouterTTS){
		"endpoint": func(r *OpenRouterTTS) { r.Endpoint = "not a url" },
		"model":    func(r *OpenRouterTTS) { r.Model = "" },
		"voice":    func(r *OpenRouterTTS) { r.Voice = "" },
		"key env":  func(r *OpenRouterTTS) { r.APIKeyEnv = "" },
		"timeout":  func(r *OpenRouterTTS) { r.Timeout = 0 },
	} {
		broken := good
		break_(&broken)
		c := Default()
		c.TTS.OpenRouter = &broken
		if err := c.Validate(); err == nil {
			t.Errorf("missing %s was accepted; it would fail only when the user spoke", name)
		}
	}
}

// The existing shape must still decode: tts.name and friends sit at the top
// level of the tts object, and an upgrade must not silently lose them.
func TestExistingTTSConfigStillDecodes(t *testing.T) {
	var c Config
	body := `{"tts":{"name":"piper","model":"/var/lib/voice/models/en_US-amy-medium.onnx","timeout":"30s","command":["piper","--model","{model}"]}}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.TTS.Name != "piper" || c.TTS.Model == "" || len(c.TTS.Command) != 3 {
		t.Errorf("decoded %+v; the existing tts fields must survive the new nesting", c.TTS)
	}
	if c.TTS.OpenRouter != nil {
		t.Error("a config with no openrouter block must not gain one")
	}
}
