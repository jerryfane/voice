package config

import (
	"strings"
	"testing"
)

func TestDefaultFastPipelineConfigurationValidates(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.STT.Resident == nil || c.STT.OpenRouter == nil || c.Brain.Jev == nil || !c.Brain.Rules {
		t.Fatalf("default fast path is incomplete: resident=%v openrouter=%v jev=%v rules=%v", c.STT.Resident != nil, c.STT.OpenRouter != nil, c.Brain.Jev != nil, c.Brain.Rules)
	}
}

func TestResidentTranscriberMustStayOnLoopback(t *testing.T) {
	c := Default()
	c.STT.Resident.Endpoint = "http://192.168.1.10:8178/inference"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback resident endpoint error = %v", err)
	}
}

func TestCloudCredentialsCannotBeSentOverPlainHTTP(t *testing.T) {
	c := Default()
	c.STT.OpenRouter.Endpoint = "http://openrouter.example/transcriptions"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP OpenRouter endpoint error = %v", err)
	}
	c = Default()
	c.Brain.Jev.Endpoint = "http://openrouter.example/decisions"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP Jev endpoint error = %v", err)
	}
}
