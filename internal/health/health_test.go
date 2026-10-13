package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCaptureQuietIsNotStalledOrFailedWake(t *testing.T) {
	m := New()
	m.WakeState("ready")
	m.Capture([]int16{0, 0})
	m.Delivered()
	m.LoopProgress()
	m.WakeProgress()
	s := m.Snapshot(time.Now())
	if s.CaptureState != "quiet" || s.Problem || !s.LastWake.IsZero() {
		t.Fatalf("quiet snapshot: %+v", s)
	}
	m.Capture([]int16{-32768, 12})
	m.Delivered()
	s = m.Snapshot(time.Now())
	if s.CaptureState != "flowing" || s.Peak != 32768 || s.LastSignal.IsZero() || s.Problem {
		t.Fatalf("signal snapshot: %+v", s)
	}
	if !strings.Contains(s.Diagnosis, "not proof of failure") {
		t.Fatal(s.Diagnosis)
	}
	s = m.Snapshot(time.Now().Add(CaptureFreshFor + time.Second))
	if s.CaptureState != "stalled" || !s.Problem {
		t.Fatalf("stalled snapshot: %+v", s)
	}
	m.Capture(nil)
	if got := m.Snapshot(time.Now()).Discontinuities; got != 1 {
		t.Fatal(got)
	}
}

func TestCaptureCanFlowWhileEventLoopFreezes(t *testing.T) {
	m := New()
	m.WakeState("ready")
	m.loop.Store(time.Now().Add(-2 * FreshFor).UnixNano())
	m.Capture([]int16{90})
	m.Delivered()
	m.WakeProgress()
	s := m.Snapshot(time.Now())
	if s.CaptureState != "flowing" || !s.Problem || !strings.Contains(s.Diagnosis, "event loop") {
		t.Fatalf("frozen loop: %+v", s)
	}
	m.LoopProgress()
	if m.Snapshot(time.Now()).Problem {
		t.Fatal("loop recovery not observed")
	}
}

func TestWakeProgressAndDetectionAreDistinct(t *testing.T) {
	m := New()
	m.Capture([]int16{90})
	m.Delivered()
	m.WakeState("ready")
	m.WakeProgress()
	if !m.Snapshot(time.Now()).LastWake.IsZero() {
		t.Fatal("progress invented a wake")
	}
	m.WakeDetected()
	if m.Snapshot(time.Now()).LastWake.IsZero() {
		t.Fatal("wake missing")
	}
	m.progress.Store(time.Now().Add(-2 * CaptureFreshFor).UnixNano())
	if s := m.Snapshot(time.Now()); !s.Problem || !strings.Contains(s.Diagnosis, "wake processing") {
		t.Fatalf("stalled wake: %+v", s)
	}
	m.WakeState("failed")
	if s := m.Snapshot(time.Now()); !s.Problem || !strings.Contains(s.Diagnosis, "detector failed") {
		t.Fatalf("failed wake: %+v", s)
	}
}

func TestPendingFrameReportsBackpressureNotProvenCaptureFailure(t *testing.T) {
	m := New()
	m.Capture([]int16{1})
	s := m.Snapshot(time.Now().Add(CaptureFreshFor + time.Second))
	if !s.Problem || !strings.Contains(s.Diagnosis, "upstream capture liveness unverified") {
		t.Fatalf("backpressure: %+v", s)
	}
}

func TestStagesTrackOverlapDeadlineAndSanitizedFailure(t *testing.T) {
	m := New()
	m.Capture([]int16{1})
	m.Delivered()
	ctx, done := m.Begin(context.Background(), "plan", time.Minute)
	_, speechDone := m.Begin(ctx, "synthesize", time.Second)
	s := m.Snapshot(time.Now())
	if len(s.Stages) != 2 || s.Stages[0].Name != "plan" {
		t.Fatalf("stages: %+v", s.Stages)
	}
	s = m.Snapshot(time.Now().Add(2 * time.Second))
	if !s.Problem || !strings.Contains(s.Diagnosis, "synthesize") {
		t.Fatalf("deadline: %+v", s)
	}
	speechDone(errors.New("secret transcript must not be published"))
	s = m.Snapshot(time.Now())
	if len(s.Stages) != 1 || s.LastFailure.Stage != "synthesize" || s.LastFailure.Kind != "failed" {
		t.Fatalf("failure: %+v", s)
	}
	done(nil)
	if len(m.Snapshot(time.Now()).Stages) != 0 {
		t.Fatal("completed stage retained")
	}
}

func TestDeadlineCompletionAndParentCancellation(t *testing.T) {
	m := New()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, done := m.Begin(ctx, "transcribe", time.Minute)
	done(nil)
	if m.Snapshot(time.Now()).LastFailure.Kind != "deadline" {
		t.Fatal("deadline was not retained")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, done = m.Begin(ctx, "plan", time.Minute)
	done(nil)
	if m.Snapshot(time.Now()).LastFailure.Kind != "cancelled" {
		t.Fatal("cancellation was not retained")
	}
}

func TestWakeStartupAndGuardAreNotUnresponsive(t *testing.T) {
	m := New()
	_, done := m.Begin(context.Background(), "wake_start", time.Minute)
	s := m.Snapshot(time.Now().Add(10 * time.Second))
	if s.Problem || s.CaptureState != "starting" {
		t.Fatalf("startup: %+v", s)
	}
	done(nil)
	m.Capture([]int16{2})
	m.Delivered()
	m.WakeProgress()
	m.WakeState("ready")
	state := "playback_guard"
	m.SetWakeGate(func() string { return state })
	s = m.Snapshot(time.Now())
	if s.Problem || s.WakeGate != state {
		t.Fatalf("guard: %+v", s)
	}
	state = "candidate_hold"
	s = m.Snapshot(time.Now())
	if s.Problem || s.WakeGate != state {
		t.Fatalf("candidate: %+v", s)
	}
}
