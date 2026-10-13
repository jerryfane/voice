package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFrontDeskDisabledByDefaultAndOldConfig(t *testing.T) {
	c := Default()
	if c.Brain.Front != nil {
		t.Fatal("front desk must default off")
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"front"`) {
		t.Fatal("default config unexpectedly enables front desk")
	}
	var installed Config
	if err := json.Unmarshal(encoded, &installed); err != nil {
		t.Fatal(err)
	}
	if installed.Brain.Front != nil || installed.Validate() != nil {
		t.Fatalf("previously installed config is no longer valid: front=%v", installed.Brain.Front)
	}
}

func TestFrontDeskConfigMustProtectCredentialsAndTransport(t *testing.T) {
	valid := func() Config {
		c := Default()
		c.Brain.Front = &Front{
			Endpoint: "https://openrouter.ai/api/v1/chat/completions",
			Model:    "small-text-model", APIKeyEnv: "OPENROUTER_API_KEY", Timeout: Duration(2 * time.Second),
		}
		return c
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid front desk rejected: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		change     func(*Front)
	}{
		{"plain HTTP", "HTTPS", func(f *Front) { f.Endpoint = "http://openrouter.ai/api/v1/chat/completions" }},
		{"URL credentials", "HTTPS", func(f *Front) { f.Endpoint = "https://user:secret@openrouter.ai/api/v1/chat/completions" }},
		{"URL query", "HTTPS", func(f *Front) { f.Endpoint += "?key=secret" }},
		{"missing model", "model", func(f *Front) { f.Model = " " }},
		{"literal secret", "api_key_env", func(f *Front) { f.APIKeyEnv = "sk-or-secret" }},
		{"missing timeout", "timeout", func(f *Front) { f.Timeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.change(c.Brain.Front)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid front config accepted or wrong error: %v", err)
			}
		})
	}
}
