package session

import (
	"context"
	"time"

	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/vad"
)

// HealthBudgets bounds entire provider cascades, not each individual attempt.
// CLI wiring sums configured attempts so their existing budgets are preserved.
type HealthBudgets struct {
	Transcribe time.Duration
	Synthesize time.Duration
	Plan       time.Duration
	Front      time.Duration
}

func budget(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
func (b HealthBudgets) transcribe() time.Duration { return budget(b.Transcribe, 2*time.Minute) }
func (b HealthBudgets) synthesize() time.Duration { return budget(b.Synthesize, 2*time.Minute) }
func (b HealthBudgets) plan() time.Duration       { return budget(b.Plan, 11*time.Minute) }
func (b HealthBudgets) front() time.Duration      { return budget(b.Front, 30*time.Second) }

func (a *Assistant) transcribe(ctx context.Context, transcriber speech.Transcriber, stage string, u vad.Utterance) (text string, err error) {
	ctx, done := a.Health.Begin(ctx, stage, a.HealthBudgets.transcribe())
	defer func() { done(err) }()
	text, err = transcriber.Transcribe(ctx, u.PCM, u.Format)
	if err == nil {
		err = ctx.Err()
	}
	return text, err
}

func (a *Assistant) frontReply(ctx context.Context, command string) (text string, err error) {
	ctx, done := a.Health.Begin(ctx, "front", a.HealthBudgets.front())
	defer func() { done(err) }()
	text, err = a.Front.Reply(ctx, command, "The previous request is still running.")
	if err == nil {
		err = ctx.Err()
	}
	return text, err
}
