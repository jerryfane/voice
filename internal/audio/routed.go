package audio

import (
	"context"
	"sync"
)

// RoutedPlayer follows a healthy capture choice when microphones and speakers
// are paired. Selection affects the next sound, never slices an in-flight WAV.
// Wrap it in SerialPlayer to serialize all callers as usual.
type RoutedPlayer struct {
	mu       sync.Mutex
	players  []Player
	selected int
	active   Player
}

func NewRoutedPlayer(players []Player) *RoutedPlayer {
	return &RoutedPlayer{players: append([]Player(nil), players...)}
}

func (p *RoutedPlayer) Select(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= 0 && index < len(p.players) {
		p.selected = index
	}
}

func (p *RoutedPlayer) play(fn func(Player) error) error {
	p.mu.Lock()
	player := p.players[p.selected]
	p.active = player
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active = nil
		p.mu.Unlock()
	}()
	return fn(player)
}

func (p *RoutedPlayer) PlayWAV(ctx context.Context, wav []byte) error {
	return p.play(func(player Player) error { return player.PlayWAV(ctx, wav) })
}

func (p *RoutedPlayer) PlayPCM(ctx context.Context, pcm []int16, f Format) error {
	return p.play(func(player Player) error { return player.PlayPCM(ctx, pcm, f) })
}

func (p *RoutedPlayer) Stop() error {
	p.mu.Lock()
	player := p.active
	p.mu.Unlock()
	if player == nil {
		return nil
	}
	return player.Stop()
}

func (p *RoutedPlayer) Describe() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.players[p.selected].Describe()
}
