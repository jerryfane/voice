package audio

import "context"

// SerialPlayer gives every Voice sound a single playback owner. A waiting sound
// observes its own context, so a cancelled request cannot play after a newer
// answer. Stop remains available while another goroutine is in PlayWAV.
type SerialPlayer struct {
	player Player
	turn   chan struct{}
}

func NewSerialPlayer(player Player) *SerialPlayer {
	return &SerialPlayer{player: player, turn: make(chan struct{}, 1)}
}

func (p *SerialPlayer) play(ctx context.Context, fn func() error) error {
	select {
	case p.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.turn }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (p *SerialPlayer) PlayWAV(ctx context.Context, wav []byte) error {
	return p.play(ctx, func() error { return p.player.PlayWAV(ctx, wav) })
}

func (p *SerialPlayer) PlayPCM(ctx context.Context, pcm []int16, f Format) error {
	return p.play(ctx, func() error { return p.player.PlayPCM(ctx, pcm, f) })
}

func (p *SerialPlayer) Stop() error      { return p.player.Stop() }
func (p *SerialPlayer) Describe() string { return p.player.Describe() }

var _ Player = (*SerialPlayer)(nil)
