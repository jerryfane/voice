package device

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSpotifyCommandArgs(t *testing.T) {
	tests := []struct {
		name       string
		cmd        Command
		current    int
		want       []string
		wantVolume int
	}{
		{
			name:       "unspecified music starts shuffled liked tracks",
			cmd:        Command{Op: OpPlay},
			current:    -1,
			want:       []string{"playback", "start", "liked", "--random"},
			wantVolume: -1,
		},
		{
			name:       "query defaults to a track",
			cmd:        Command{Op: OpPlay, Args: map[string]any{"query": "So What"}},
			current:    -1,
			want:       []string{"playback", "start", "track", "--name", "So What"},
			wantVolume: -1,
		},
		{
			name:       "artist starts an artist context",
			cmd:        Command{Op: OpPlay, Args: map[string]any{"query": "Miles Davis", "type": "artist"}},
			current:    -1,
			want:       []string{"playback", "start", "context", "--name", "Miles Davis", "artist"},
			wantVolume: -1,
		},
		{
			name:       "pause",
			cmd:        Command{Op: OpPause},
			current:    -1,
			want:       []string{"playback", "pause"},
			wantVolume: -1,
		},
		{
			name:       "seven maps to seventy percent",
			cmd:        Command{Op: OpVolume, Args: map[string]any{"level": 7}},
			current:    -1,
			want:       []string{"playback", "volume", "70"},
			wantVolume: 70,
		},
		{
			name:       "volume up is one scale step",
			cmd:        Command{Op: OpVolumeUp},
			current:    70,
			want:       []string{"playback", "volume", "80"},
			wantVolume: 80,
		},
		{
			name:       "volume down clamps at zero",
			cmd:        Command{Op: OpVolumeDown},
			current:    0,
			want:       []string{"playback", "volume", "0"},
			wantVolume: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, volume, err := spotifyCommandArgs(tt.cmd, tt.current)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %q, want %q", got, tt.want)
			}
			if volume != tt.wantVolume {
				t.Fatalf("expected volume = %d, want %d", volume, tt.wantVolume)
			}
		})
	}
}

func TestSpotifyRejectsInvalidPlaybackArguments(t *testing.T) {
	_, _, err := spotifyCommandArgs(Command{Op: OpPlay, Args: map[string]any{
		"query": "Blue Train",
		"type":  "podcast",
	}}, -1)
	if err == nil || !strings.Contains(err.Error(), "track, album, artist, or playlist") {
		t.Fatalf("context error = %v, want supported-context explanation", err)
	}

	_, _, err = spotifyCommandArgs(Command{Op: OpVolume, Args: map[string]any{"level": 11}}, -1)
	if err == nil || !strings.Contains(err.Error(), "0-10") {
		t.Fatalf("volume error = %v, want 0-10 boundary", err)
	}
}

// Reconnecting a paused but active endpoint resumes playback. Volume changes
// must use the existing endpoint rather than waking it first.
func TestSpotifyVolumeDoesNotReconnectPausedActiveDevice(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "spotify-player")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$SPOTIFY_LOG"
if [ "$*" = "get key playback" ]; then
  printf '%s\n' '{"device":{"name":"spotify-player","volume_percent":40},"progress_ms":1000,"is_playing":false,"item":{"id":"track","name":"Paused Track","artists":[]}}'
fi
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTIFY_LOG", logPath)

	s := NewSpotify("spotify", "spotify-player", []string{script})
	state, err := s.Apply(context.Background(), Command{Op: OpVolume, Args: map[string]any{"level": 4}})
	if err != nil {
		t.Fatal(err)
	}
	if playing, _ := state["playing"].(bool); playing {
		t.Fatalf("volume change resumed playback: %v", state)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "connect --name") {
		t.Fatalf("volume change reconnected the active device:\n%s", calls)
	}
}
