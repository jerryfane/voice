package session

import (
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

func TestBuildFrontDeskIsOptInAndNeedsEnvironmentKey(t *testing.T) {
	c := config.Default()
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.Detector = "stt"
	if got := Build(c).Front; got != nil {
		t.Fatal("default build enabled the remote front desk")
	}
	c.Brain.Front = &config.Front{
		Endpoint: "https://openrouter.ai/api/v1/chat/completions",
		Model:    "small-text-model", APIKeyEnv: "VOICE_FRONT_TEST_API_KEY", Timeout: config.Duration(time.Second),
	}
	t.Setenv("VOICE_FRONT_TEST_API_KEY", "")
	if got := Build(c).Front; got != nil {
		t.Fatal("front desk enabled without credentials")
	}
	t.Setenv("VOICE_FRONT_TEST_API_KEY", "test-secret")
	if got := Build(c).Front; got == nil {
		t.Fatal("configured front desk was not wired")
	}
}
