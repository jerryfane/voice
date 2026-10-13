package vad

import (
	"context"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

type keywordHealthObserver struct {
	progress int
	detected int
}

func (h *keywordHealthObserver) WakeProgress() { h.progress++ }
func (h *keywordHealthObserver) WakeDetected() { h.detected++ }

func TestKeywordHealthDistinguishesGuardProgressFromLocalWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2}, &fakeKeywordDetector{matchAt: 1}, nil)
	k.SetCaptureBoundary(boundary)
	health := &keywordHealthObserver{}
	k.SetHealthObserver(health)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	if got := k.HealthState(); got != "wake_listening" {
		t.Fatalf("initial gate = %q", got)
	}
	k.Suspend()
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	if got := k.HealthState(); got != "playback_guard" {
		t.Fatalf("active playback gate = %q", got)
	}
	k.ResumeAfter(time.Hour)
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	if got := k.HealthState(); got != "playback_guard" {
		t.Fatalf("hardware tail gate = %q", got)
	}
	if health.progress != 2 || health.detected != 0 {
		t.Fatalf("guarded capture progress=%d local wakes=%d", health.progress, health.detected)
	}
	k.ResumeAfter(0)
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(2000, 4), false)
	if got := k.HealthState(); got != "candidate_hold" {
		t.Fatalf("locally detected wake gate = %q", got)
	}
	close(in)
	for range out {
	}
	if health.progress != 3 || health.detected != 1 {
		t.Fatalf("fresh capture progress=%d local wakes=%d", health.progress, health.detected)
	}
}

func TestKeywordHealthReportsProgressWithoutRecoveryBoundary(t *testing.T) {
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 2}, &fakeKeywordDetector{}, nil)
	health := &keywordHealthObserver{}
	k.SetHealthObserver(health)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16, 3)
	for range 3 {
		in <- sampleFrame(0, 4)
	}
	close(in)
	for range k.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1}) {
	}
	if health.progress != 3 || health.detected != 0 {
		t.Fatalf("unbounded capture progress=%d local wakes=%d", health.progress, health.detected)
	}
}
