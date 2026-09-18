package device

import (
	"reflect"
	"strings"
	"testing"
)

func TestSpotifyCommandArgs(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		want []string
	}{
		{
			name: "unspecified music starts shuffled liked tracks",
			cmd:  Command{Op: OpPlay},
			want: []string{"playback", "start", "liked", "--random"},
		},
		{
			name: "query defaults to a track",
			cmd:  Command{Op: OpPlay, Args: map[string]any{"query": "So What"}},
			want: []string{"playback", "start", "track", "--name", "So What"},
		},
		{
			name: "artist starts an artist context",
			cmd:  Command{Op: OpPlay, Args: map[string]any{"query": "Miles Davis", "type": "artist"}},
			want: []string{"playback", "start", "context", "--name", "Miles Davis", "artist"},
		},
		{
			name: "pause",
			cmd:  Command{Op: OpPause},
			want: []string{"playback", "pause"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := spotifyCommandArgs(tt.cmd)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpotifyPlayRejectsAnUnknownContextInsteadOfGuessing(t *testing.T) {
	_, err := spotifyCommandArgs(Command{Op: OpPlay, Args: map[string]any{
		"query": "Blue Train",
		"type":  "podcast",
	}})
	if err == nil || !strings.Contains(err.Error(), "track, album, artist, or playlist") {
		t.Fatalf("error = %v, want supported-context explanation", err)
	}
}
