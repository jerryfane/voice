// Package feedback gives immediate local acknowledgement when the wake gate
// accepts a phrase: a listening light and a short sound. Everything here runs
// in the session layer with no model call, so feedback is instant and works
// when the network or the brain is unavailable.
package feedback

import (
	"context"
	"log"
	"strconv"

	"github.com/jerryfane/voice/internal/audio"
)

// State is what the physical indicator should show.
type State int

const (
	// Idle means the wake gate is armed but nothing has been accepted.
	Idle State = iota
	// Listening means an accepted wake phrase is being captured or handled.
	Listening
)

func (s State) String() string {
	if s == Listening {
		return "listening"
	}
	return "idle"
}

// Indicator drives a physical listening light. Implementations must be safe to
// call from the session loop and must not block for long: the light has to
// change within a perceptible fraction of a second of wake acceptance.
type Indicator interface {
	Set(State) error
	Describe() string
}

// Nop is the indicator used when no supported hardware is configured. Voice
// keeps accepting commands without a light.
type Nop struct{}

func (Nop) Set(State) error  { return nil }
func (Nop) Describe() string { return "no indicator (no supported device configured)" }

// Notifier couples the indicator and the acknowledgement sound. A nil *Notifier
// is valid and does nothing, so callers never need a branch.
type Notifier struct {
	Indicator Indicator
	Player    audio.Player
	// Sound is pre-rendered PCM in Format. Nil disables the sound.
	Sound  []int16
	Format audio.Format
	Logger *log.Logger
}

func (n *Notifier) logf(f string, v ...any) {
	if n != nil && n.Logger != nil {
		n.Logger.Printf(f, v...)
	}
}

// Accepted runs the feedback for one accepted wake phrase: the light changes
// first (it is the fastest signal and the one the user is looking at), then the
// sound plays. Failures are logged, never returned: feedback must not stop a
// command from being handled.
func (n *Notifier) Accepted(ctx context.Context) {
	if n == nil {
		return
	}
	n.set(Listening)
	if len(n.Sound) == 0 || n.Player == nil {
		return
	}
	if err := n.Player.PlayPCM(ctx, n.Sound, n.Format); err != nil && ctx.Err() == nil {
		n.logf("feedback sound: %v", err)
	}
}

// Restore returns the indicator to idle. It takes no context so it still runs
// after cancellation, timeout or shutdown, and it never plays a sound.
func (n *Notifier) Restore() {
	if n == nil {
		return
	}
	n.set(Idle)
}

func (n *Notifier) set(s State) {
	if n.Indicator == nil {
		return
	}
	if err := n.Indicator.Set(s); err != nil {
		n.logf("feedback indicator %s: %v", s, err)
	}
}

// Describe reports the configured feedback for `voice doctor`.
func (n *Notifier) Describe() string {
	if n == nil || n.Indicator == nil {
		return "disabled"
	}
	d := n.Indicator.Describe()
	if len(n.Sound) == 0 {
		return d + "; sound off"
	}
	ms := len(n.Sound) * 1000 / max(n.Format.SampleRate, 1)
	return d + "; " + strconv.Itoa(ms) + " ms sound"
}
