package vad

import (
	"context"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/feedback"
)

// A pulse remains in flight until the test releases it, just as the real
// player must finish before its tail guard and quiet listening gap begin.
type gapPlayer struct {
	started chan struct{}
	release chan struct{}
}

func (p *gapPlayer) PlayPCM(ctx context.Context, _ []int16, _ audio.Format) error {
	select {
	case p.started <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *gapPlayer) PlayWAV(ctx context.Context, _ []byte) error {
	return p.PlayPCM(ctx, nil, audio.Default())
}
func (*gapPlayer) Stop() error      { return nil }
func (*gapPlayer) Describe() string { return "controlled thinking pulse" }

func TestThinkingGapCandidateOutlastsNextPulseAndExcludesGuardAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 3, Silence: 2, PreRoll: 2}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	k.SetInFlightWindow(time.Now().Add(time.Minute), 2)
	if got := k.HealthState(); got != "authorized_listening" {
		t.Fatalf("open conversation gate = %q", got)
	}
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	defer func() {
		cancel()
		for range out {
		}
		k.Close() // discard: Keyword.Close has no return value.
	}()
	p := &gapPlayer{started: make(chan struct{}, 2), release: make(chan struct{})}
	resumed := make(chan struct{}, 2)
	const gap = 200 * time.Millisecond
	n := &feedback.Notifier{
		Player: p, Working: []int16{1}, Format: audio.Default(),
		ThinkingDelay: time.Millisecond, ThinkingGap: gap,
		BeforeThinking: k.Suspend, CanThink: k.CanThink,
		AfterThinking: func() {
			// Advance the tail explicitly below; no wall-clock assumption
			// about when these synthetic hardware frames reach capture.
			k.ResumeAfter(time.Hour)
			resumed <- struct{}{}
		},
	}
	stop := n.Thinking(ctx)
	defer stop()
	await := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	assertQuiet := func() {
		t.Helper()
		select {
		case <-p.started:
			t.Fatal("next pulse interrupted candidate or queued speech")
		case <-time.After(2 * gap):
		case <-ctx.Done():
			t.Fatal("timed out while holding speech")
		}
	}
	await(p.started, "audible thinking")
	for range 3 {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	}
	p.release <- struct{}{}
	await(resumed, "playback completion")
	for range 3 {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	}
	k.ResumeAfter(0)
	// Onset is not yet a full MinSpeech candidate. It must nevertheless
	// hold the next pulse past the end of the ordinary inter-pulse gap.
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(2000, 4), false)
	assertQuiet()
	for _, value := range []int16{2000, 2000, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(value, 4), false)
	}
	assertQuiet()
	var u Utterance
	select {
	case u = <-out:
	case <-ctx.Done():
		t.Fatal("onset in the listening gap did not finish")
	}
	if u.Keyword != "IN_FLIGHT" || len(u.PCM) != 20 {
		t.Fatalf("gap speech lost its onset or authorization: %+v", u)
	}
	for _, sample := range u.PCM {
		if sample == 3000 {
			t.Fatal("thinking playback or hardware tail entered the utterance")
		}
	}
	if k.CanThink() {
		t.Fatal("receiving output released the queued-speech hold before acceptance")
	}
	if got := k.HealthState(); got != "queued" {
		t.Fatalf("unaccepted speech gate = %q", got)
	}
	if !k.Accepts(u) {
		t.Fatal("completed gap candidate lost authorization")
	}
	await(p.started, "thinking resuming after speech acceptance")
	stop()
}

func TestThinkingGapDoesNotRenewAbsoluteAuthorization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 3, Silence: 2}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	k.SetInFlightWindow(time.Now().Add(time.Minute), 1)
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	k.Suspend()
	k.ResumeAfter(0)
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(2000, 4), false)
	if k.CanThink() {
		t.Fatal("candidate did not hold thinking")
	}
	// Expire the same authorization generation while its candidate is held.
	k.busyMu.Lock()
	k.busyUntil = time.Now().Add(-time.Second)
	k.busyMu.Unlock()
	for i, value := range []int16{2000, 2000, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(value, 4), i == 3)
	}
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("a listening-gap candidate extended the absolute deadline")
	}
}
