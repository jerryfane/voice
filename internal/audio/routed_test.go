package audio

import (
	"context"
	"sync/atomic"
	"testing"
)

type routedHeldPlayer struct {
	heldPlayer
	stops atomic.Int32
	name  string
}

func (p *routedHeldPlayer) Stop() error      { p.stops.Add(1); return nil }
func (p *routedHeldPlayer) Describe() string { return p.name }

func TestCapturePlaybackRouteChangesOnlyBetweenSounds(t *testing.T) {
	first := &routedHeldPlayer{heldPlayer: heldPlayer{entered: make(chan struct{}, 1), release: make(chan struct{})}, name: "preferred"}
	second := &routedHeldPlayer{heldPlayer: heldPlayer{entered: make(chan struct{}, 1), release: make(chan struct{})}, name: "fallback"}
	routed := NewRoutedPlayer([]Player{first, second})
	player := NewSerialPlayer(routed)
	done := make(chan error, 1)
	go func() { done <- player.PlayWAV(context.Background(), []byte{1}) }()
	<-first.entered
	routed.Select(1)
	if err := player.Stop(); err != nil {
		t.Fatal(err)
	}
	if first.stops.Load() != 1 || second.stops.Load() != 0 {
		t.Fatal("Stop targeted newly selected route instead of in-flight sound")
	}
	select {
	case <-second.entered:
		t.Fatal("route switch restarted in-flight WAV")
	default:
	}
	close(first.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() { done <- player.PlayPCM(context.Background(), []int16{2}, Default()) }()
	<-second.entered
	close(second.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := player.Describe(); got != "fallback" {
		t.Fatalf("route=%q", got)
	}
	routed.Select(0)
	if got := player.Describe(); got != "preferred" {
		t.Fatalf("preferred return route=%q", got)
	}
}
