package vad

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

// Sending the next frame only after Wait mirrors the recovery capture protocol.
// Timeouts bound failures; no test depends on scheduling delays or sleeps.
func sendBoundaryFrame(t *testing.T, ctx context.Context, in chan<- []int16, boundary *audio.CaptureBoundary, pcm []int16, wantIdle bool) {
	t.Helper()
	select {
	case in <- pcm:
	case <-ctx.Done():
		t.Fatal("capture frame was not consumed")
	}
	idle, ok := boundary.Wait(ctx)
	if !ok {
		t.Fatal("capture frame was not acknowledged")
	}
	if idle != wantIdle {
		t.Fatalf("idle = %v, want %v, frame = %v", idle, wantIdle, pcm)
	}
}

func requireBoundaryUtterance(t *testing.T, out <-chan Utterance, values ...int16) {
	t.Helper()
	u, ok := <-out
	if !ok {
		t.Fatal("no utterance")
	}
	var want []int16
	for _, value := range values {
		want = append(want, frame(value, 4)...)
	}
	if !slices.Equal(u.PCM, want) {
		t.Fatalf("utterance PCM = %v, want %v", u.PCM, want)
	}
	if _, ok := <-out; ok {
		t.Fatal("unexpected partial or extra utterance")
	}
}

func TestEnergyCaptureBoundaryRetainsSpeechAcrossShortPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	e := NewEnergy(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 3, PreRoll: 1, MaxUtterance: 30})
	e.SetCaptureBoundary(boundary)
	in := make(chan []int16)
	out := e.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	values := []int16{0, 0, 0, 1000, 1100, 0, 0, 1200, 0, 0, 0}
	for i, value := range values {
		sendBoundaryFrame(t, ctx, in, boundary, frame(value, 4), i == 2 || i == len(values)-1)
	}
	close(in)
	requireBoundaryUtterance(t, out, 0, 1000, 1100, 0, 0, 1200, 0, 0, 0)
}

func TestEnergyDiscontinuityDiscardsCandidateCommandAndPreRoll(t *testing.T) {
	for _, withBoundary := range []bool{false, true} {
		for _, old := range [][]int16{{1000}, {1000, 1100}, {100, 200}} {
			for _, marker := range [][]int16{nil, {}} {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				e := NewEnergy(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 3, PreRoll: 2, MaxUtterance: 30})
				in := make(chan []int16, 16)
				var boundary *audio.CaptureBoundary
				if withBoundary {
					boundary = audio.NewCaptureBoundary()
					e.SetCaptureBoundary(boundary)
					in = make(chan []int16)
				}
				out := e.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
				send := func(pcm []int16, idle bool) {
					if boundary != nil {
						sendBoundaryFrame(t, ctx, in, boundary, pcm, idle)
					} else {
						in <- pcm
					}
				}
				for _, value := range old {
					send(frame(value, 4), false)
				}
				send(marker, false)
				for i, value := range []int16{2000, 2100, 0, 0, 0} {
					send(frame(value, 4), i == 4)
				}
				close(in)
				requireBoundaryUtterance(t, out, 2000, 2100, 0, 0, 0)
				cancel()
			}
		}
	}
}

func TestKeywordCaptureBoundaryRequiresQuietWhileArmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	for i, value := range []int16{0, 0, 0, 1000, 0, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, frame(value, 4), i == 2 || i == 6)
	}
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("unmatched partial wake emitted an utterance")
	}
}

func TestKeywordCaptureBoundaryRetainsSpeechAcrossShortPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3, PreRoll: 1, MaxUtterance: 30}, &fakeKeywordDetector{matchAt: 1}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	values := []int16{1000, 1100, 1200, 0, 0, 1300, 0, 0, 0}
	for i, value := range values {
		sendBoundaryFrame(t, ctx, in, boundary, frame(value, 4), i == len(values)-1)
	}
	close(in)
	requireBoundaryUtterance(t, out, values...)
}

func TestKeywordCaptureBoundaryFollowUpAndSuspendStayBusy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3, PreRoll: 1}, &fakeKeywordDetector{matchAt: 1}, nil)
	k.SetCaptureBoundary(boundary)
	acknowledged, restored := 0, 0
	k.SetFollowUp(5, func(context.Context) { acknowledged++ }, func() { restored++ })
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	sendBoundaryFrame(t, ctx, in, boundary, frame(1000, 4), false)
	sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false) // standalone wake, feedback
	sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false) // hardware tail
	for i := range 5 {
		sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), i == 4)
	}
	k.Suspend()
	for range 4 {
		sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false)
	}
	k.ResumeAfter(0)
	// A discontinuity resets the detector and all partial follow-up state.
	sendBoundaryFrame(t, ctx, in, boundary, nil, false)
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("standalone wake or suspended audio emitted an utterance")
	}
	if acknowledged != 1 || restored != 1 {
		t.Fatalf("feedback acknowledgements/restores = %d/%d, want 1/1", acknowledged, restored)
	}
}

func TestKeywordDiscontinuityDiscardsDecoderCommandAndPreRoll(t *testing.T) {
	for _, withBoundary := range []bool{false, true} {
		for _, old := range [][]int16{{1000}, {1000, 1100}, {1000, 1100, 1200}} {
			for _, marker := range [][]int16{nil, {}} {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				detector := &fakeKeywordDetector{matchAt: 2}
				k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3, PreRoll: 2, MaxUtterance: 30}, detector, nil)
				in := make(chan []int16, 16)
				var boundary *audio.CaptureBoundary
				if withBoundary {
					boundary = audio.NewCaptureBoundary()
					k.SetCaptureBoundary(boundary)
					in = make(chan []int16)
				}
				if err := k.Start(); err != nil {
					t.Fatal(err)
				}
				out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
				send := func(pcm []int16, idle bool) {
					if boundary != nil {
						sendBoundaryFrame(t, ctx, in, boundary, pcm, idle)
					} else {
						in <- pcm
					}
				}
				for _, value := range old {
					send(frame(value, 4), false)
				}
				send(marker, false)
				values := []int16{2000, 2100, 2200, 2300, 0, 0, 0}
				for i, value := range values {
					send(frame(value, 4), i == len(values)-1)
				}
				close(in)
				requireBoundaryUtterance(t, out, values...)
				if detector.resets != 1 {
					t.Fatalf("decoder resets = %d, want 1", detector.resets)
				}
				k.Close() // discard: Keyword.Close has no return value.
				cancel()
			}
		}
	}
}

func TestKeywordCaptureBoundaryAcknowledgesDetectorFailure(t *testing.T) {
	for _, resetFailure := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		detector := &fakeKeywordDetector{}
		pcm := frame(1000, 4)
		if resetFailure {
			detector.resetErr = errors.New("reset failed")
			pcm = nil
		} else {
			detector.err = errors.New("decode failed")
		}
		boundary := audio.NewCaptureBoundary()
		k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3}, detector, nil)
		k.SetCaptureBoundary(boundary)
		if err := k.Start(); err != nil {
			t.Fatal(err)
		}
		in := make(chan []int16)
		out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
		sendBoundaryFrame(t, ctx, in, boundary, pcm, false)
		if _, ok := <-out; ok {
			t.Fatal("detector failure emitted an utterance")
		}
		k.Close() // discard: Keyword.Close has no return value.
		cancel()
	}
}

func TestKeywordDiscontinuityClosesFollowUpAuthorization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	detector := &fakeKeywordDetector{matchAt: 1}
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, Silence: 3, PreRoll: 1}, detector, nil)
	k.SetCaptureBoundary(boundary)
	restored := 0
	k.SetFollowUp(20, func(context.Context) {}, func() { restored++ })
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	sendBoundaryFrame(t, ctx, in, boundary, frame(1000, 4), false)
	sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false)
	sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false)
	sendBoundaryFrame(t, ctx, in, boundary, nil, false)
	detector.matchAt = 0 // no new wake phrase on the replacement microphone
	for i, value := range []int16{2000, 2100, 0, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, frame(value, 4), i == 4)
	}
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("follow-up authorization crossed a microphone discontinuity")
	}
	if restored != 1 || detector.resets != 1 {
		t.Fatalf("restores/resets = %d/%d, want 1/1", restored, detector.resets)
	}
}

func TestKeywordCaptureBoundaryAcknowledgesInFlightBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 3}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	k.SetInFlightWindow(time.Now().Add(time.Hour), 2)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	for range 4 {
		sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false)
	}
	sendBoundaryFrame(t, ctx, in, boundary, frame(1000, 4), false)
	sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), false) // rejected busy probe
	sendBoundaryFrame(t, ctx, in, boundary, frame(1000, 4), false)
	k.SetInFlightWindow(time.Time{}, 0)
	sendBoundaryFrame(t, ctx, in, boundary, frame(1000, 4), false) // revoked busy probe
	for i := range 3 {
		sendBoundaryFrame(t, ctx, in, boundary, frame(0, 4), i == 2)
	}
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("incomplete in-flight candidate emitted an utterance")
	}
}
