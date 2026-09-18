package device

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/proc"
)

// Spotify controls the local spotify_player daemon through its loopback CLI
// socket. The daemon owns credentials and audio; Voice only sends playback
// commands and reads the resulting state.
type Spotify struct {
	Name       string
	DeviceName string
	Command    []string
	Timeout    time.Duration
}

func NewSpotify(id, deviceName string, command []string) *Spotify {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "spotify-player"
	}
	return &Spotify{Name: id, DeviceName: deviceName, Command: append([]string(nil), command...), Timeout: 12 * time.Second}
}

func (s *Spotify) ID() string   { return s.Name }
func (s *Spotify) Kind() string { return "music" }
func (s *Spotify) Describe() string {
	return fmt.Sprintf("Spotify Connect device %q; play accepts optional query/type args, volume uses integer level 0-10", s.DeviceName)
}
func (s *Spotify) Capabilities() []Op {
	return []Op{OpPlay, OpResume, OpPause, OpNext, OpPrevious, OpVolume, OpVolumeUp, OpVolumeDown}
}

func (s *Spotify) run(ctx context.Context, args ...string) ([]byte, error) {
	argv := make([]string, 0, len(s.Command)+len(args))
	argv = append(argv, s.Command...)
	argv = append(argv, args...)
	r, err := proc.Run(ctx, argv, nil, s.Timeout)
	if err != nil {
		return nil, fmt.Errorf("spotify_player %s: %w", strings.Join(args, " "), err)
	}
	return r.Stdout, nil
}

type spotifyPlayback struct {
	Device *struct {
		Name          string `json:"name"`
		VolumePercent int    `json:"volume_percent"`
	} `json:"device"`
	Item *struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
	} `json:"item"`
	ProgressMS int  `json:"progress_ms"`
	IsPlaying  bool `json:"is_playing"`
}

func (s *Spotify) Read(ctx context.Context) (State, error) {
	out, err := s.run(ctx, "get", "key", "playback")
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "null" {
		return State{"playing": false, "active": false}, nil
	}
	var p spotifyPlayback
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("decode Spotify playback state: %w", err)
	}
	state := State{"playing": p.IsPlaying, "active": p.Device != nil, "progress_ms": p.ProgressMS}
	if p.Device != nil {
		state["device"] = p.Device.Name
		state["volume"] = p.Device.VolumePercent
	}
	if p.Item != nil {
		state["track_id"] = p.Item.ID
		state["track"] = p.Item.Name
		artists := make([]string, 0, len(p.Item.Artists))
		for _, artist := range p.Item.Artists {
			artists = append(artists, artist.Name)
		}
		if len(artists) > 0 {
			state["artists"] = strings.Join(artists, ", ")
		}
	}
	return state, nil
}

func (s *Spotify) Apply(ctx context.Context, cmd Command) (State, error) {
	if !Supports(s, cmd.Op) {
		return nil, fmt.Errorf("Spotify does not support %q", cmd.Op)
	}
	// Spotify's Web API returns 404 when no playback device is active, even
	// though the integrated librespot endpoint is online. Read first and only
	// activate the daemon when necessary: reconnecting an already-active but
	// paused device resumes it, so doing that for a volume change unexpectedly
	// starts music.
	before, err := s.Read(ctx)
	if err != nil {
		return nil, err
	}
	active, _ := before["active"].(bool)
	isVolume := cmd.Op == OpVolume || cmd.Op == OpVolumeUp || cmd.Op == OpVolumeDown
	if isVolume && !active {
		return nil, fmt.Errorf("Spotify has no active playback device; start music before changing its volume")
	}
	if cmd.Op != OpPause && !active {
		if _, err := s.run(ctx, "connect", "--name", s.DeviceName); err != nil {
			return nil, err
		}
		before, err = s.Read(ctx)
		if err != nil {
			return nil, err
		}
	}

	var previousTrack string
	currentVolume := -1
	if cmd.Op == OpNext || cmd.Op == OpPrevious {
		previousTrack, _ = before["track_id"].(string)
	}
	if volume, ok := before["volume"].(int); ok {
		currentVolume = volume
	}
	args, expectedVolume, err := spotifyCommandArgs(cmd, currentVolume)
	if err != nil {
		return nil, err
	}
	if _, err := s.run(ctx, args...); err != nil {
		return nil, err
	}
	return s.waitForApplied(ctx, cmd.Op, previousTrack, expectedVolume)
}

func spotifyCommandArgs(cmd Command, currentVolume int) ([]string, int, error) {
	switch cmd.Op {
	case OpPlay:
		query, err := optionalStringArg(cmd.Args, "query")
		if err != nil {
			return nil, -1, err
		}
		kind, err := optionalStringArg(cmd.Args, "type")
		if err != nil {
			return nil, -1, err
		}
		if query == "" {
			if kind != "" {
				return nil, -1, fmt.Errorf("Spotify play type requires a query")
			}
			return []string{"playback", "start", "liked", "--random"}, -1, nil
		}
		kind = strings.ToLower(kind)
		if kind == "" || kind == "song" {
			kind = "track"
		}
		switch kind {
		case "track":
			return []string{"playback", "start", "track", "--name", query}, -1, nil
		case "album", "artist", "playlist":
			return []string{"playback", "start", "context", "--name", query, kind}, -1, nil
		default:
			return nil, -1, fmt.Errorf("Spotify play type must be track, album, artist, or playlist, got %q", kind)
		}
	case OpResume:
		return []string{"playback", "play"}, -1, nil
	case OpPause:
		return []string{"playback", "pause"}, -1, nil
	case OpNext:
		return []string{"playback", "next"}, -1, nil
	case OpPrevious:
		return []string{"playback", "previous"}, -1, nil
	case OpVolume:
		level, err := number(cmd.Args, "level", -1)
		if err != nil {
			return nil, -1, err
		}
		if level < 0 || level > 10 {
			return nil, -1, fmt.Errorf("Spotify volume level must be 0-10")
		}
		percent := level * 10
		return []string{"playback", "volume", strconv.Itoa(percent)}, percent, nil
	case OpVolumeUp, OpVolumeDown:
		if currentVolume < 0 {
			return nil, -1, fmt.Errorf("Spotify did not report its current volume")
		}
		percent := currentVolume
		if cmd.Op == OpVolumeUp {
			percent = min(100, percent+10)
		} else {
			percent = max(0, percent-10)
		}
		return []string{"playback", "volume", strconv.Itoa(percent)}, percent, nil
	default:
		return nil, -1, fmt.Errorf("Spotify does not support %q", cmd.Op)
	}
}

func optionalStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("Spotify %s must be a string", key)
	}
	return strings.TrimSpace(s), nil
}

func (s *Spotify) waitForApplied(ctx context.Context, op Op, previousTrack string, expectedVolume int) (State, error) {
	deadline := time.NewTimer(s.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last State
	for {
		state, err := s.Read(ctx)
		if err == nil {
			last = state
			playing, _ := state["playing"].(bool)
			track, _ := state["track_id"].(string)
			volume, _ := state["volume"].(int)
			switch op {
			case OpPause:
				if !playing {
					return state, nil
				}
			case OpPlay, OpResume:
				if playing {
					return state, nil
				}
			case OpNext, OpPrevious:
				if previousTrack == "" || (track != "" && track != previousTrack) {
					return state, nil
				}
			case OpVolume, OpVolumeUp, OpVolumeDown:
				if volume == expectedVolume {
					return state, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("Spotify did not apply %s before timeout; last state: %v", op, last)
		case <-tick.C:
		}
	}
}
