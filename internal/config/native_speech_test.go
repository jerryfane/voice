package config

import "testing"

func TestNativeSpeechAllowsProviderDefaultVoice(t *testing.T) {
	c := Default()
	c.TTS.OpenRouter.Endpoint = "https://openrouter.ai/api/v1/audio/speech"
	c.TTS.OpenRouter.Model = "fish-audio/drama-3"
	c.TTS.OpenRouter.Voice = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("native provider-default voice rejected: %v", err)
	}
	c.TTS.OpenRouter.Endpoint = "https://openrouter.ai/api/v1/chat/completions"
	if err := c.Validate(); err == nil {
		t.Fatal("chat audio accepted a missing required voice")
	}
}
