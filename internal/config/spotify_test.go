package config

import (
	"strings"
	"testing"
)

func TestSpotifyConfigRequiresACommandAndUniqueDeviceID(t *testing.T) {
	c := Default()
	c.Spotify = &Spotify{ID: "spotify"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "has no command") {
		t.Fatalf("missing command error = %v", err)
	}

	c.Spotify.Command = Command{"spotify_player"}
	c.Lights = []Light{{ID: "spotify", Host: "127.0.0.1"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate device id") {
		t.Fatalf("duplicate ID error = %v", err)
	}

	c.Spotify.ID = "music"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid Spotify config: %v", err)
	}
}
