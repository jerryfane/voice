package audio

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type heldPlayer struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *heldPlayer) PlayWAV(context.Context, []byte) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.entered <- struct{}{}
	<-p.release
	return nil
}
func (p *heldPlayer) PlayPCM(ctx context.Context, _ []int16, _ Format) error {
	return p.PlayWAV(ctx, nil)
}
func (*heldPlayer) Stop() error      { return nil }
func (*heldPlayer) Describe() string { return "held player" }

func TestSerialPlayerCancelledQueueNeverPlays(t *testing.T) {
	inner := &heldPlayer{entered: make(chan struct{}, 2), release: make(chan struct{})}
	player := NewSerialPlayer(inner)
	first := make(chan error, 1)
	go func() { first <- player.PlayWAV(context.Background(), nil) }()
	<-inner.entered

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- player.PlayPCM(ctx, []int16{1}, Default()) }()
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued playback error = %v, want cancellation", err)
	}
	close(inner.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if inner.calls != 1 {
		t.Fatalf("played %d sounds, want only the first", inner.calls)
	}
}
