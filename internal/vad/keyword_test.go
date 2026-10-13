package vad

import (
	"context"
	"errors"
	"github.com/jerryfane/voice/internal/audio"
	"testing"
	"time"
)

type fakeKeywordDetector struct {
	matchAt  int
	calls    int
	started  bool
	closed   bool
	err      error
	resets   int
	resetErr error
}

func (*fakeKeywordDetector) Name() string              { return "fake keyword detector" }
func (*fakeKeywordDetector) Available() (bool, string) { return true, "available" }
func (d *fakeKeywordDetector) Start() error            { d.started = true; return nil }
func (d *fakeKeywordDetector) Close()                  { d.closed = true }
func (d *fakeKeywordDetector) Reset() error {
	d.resets++
	d.calls = 0
	return d.resetErr
}
func (d *fakeKeywordDetector) Accept(int, []int16) (string, error) {
	d.calls++
	if d.err != nil {
		return "", d.err
	}
	if d.calls == d.matchAt {
		return "HEY_VOICE", nil
	}
	return "", nil
}

func TestInFlightWindowBoundsWakeFreeCapture(t *testing.T) {
	detector := &fakeKeywordDetector{}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2, MaxUtterance: 10}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close only releases the fake detector and has no result.
	segmenter.SetInFlightWindow(time.Now().Add(time.Minute), 1)

	in := make(chan []int16, 12)
	for range 3 {
		in <- sampleFrame(2000, 4)
	}
	in <- sampleFrame(0, 4)
	in <- sampleFrame(0, 4)
	for range 3 {
		in <- sampleFrame(2000, 4)
	}
	in <- sampleFrame(0, 4)
	in <- sampleFrame(0, 4)
	close(in)
	out := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
	u, ok := <-out
	if !ok || !u.WakeMatched || !u.WakeAcknowledged || u.Keyword != "IN_FLIGHT" {
		t.Fatalf("authorized in-flight utterance = %+v, present=%v", u, ok)
	}
	if _, ok := <-out; ok {
		t.Fatal("one-turn window accepted a second wake-free command")
	}
}

func TestInFlightCommandCompletesWhenCaptureCloses(t *testing.T) {
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2, MaxUtterance: 10}, &fakeKeywordDetector{}, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close only releases the fake detector and has no result.
	segmenter.SetInFlightWindow(time.Now().Add(time.Minute), 1)
	in := make(chan []int16, 2)
	in <- sampleFrame(2000, 4)
	in <- sampleFrame(2000, 4)
	close(in)
	out := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
	u, ok := <-out
	if !ok || u.Keyword != "IN_FLIGHT" || !u.WakeMatched {
		t.Fatalf("closed capture lost authorized turn: %+v, present=%v", u, ok)
	}
	if _, ok := <-out; ok {
		t.Fatal("closed capture emitted a second turn")
	}
}

func TestExpiredInFlightWindowDoesNotUploadAmbientAudio(t *testing.T) {
	detector := &fakeKeywordDetector{}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 1, Silence: 2}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close only releases the fake detector and has no result.
	segmenter.SetInFlightWindow(time.Now().Add(-time.Second), 2)
	in := make(chan []int16, 4)
	for range 4 {
		in <- sampleFrame(2000, 4)
	}
	close(in)
	if _, ok := <-segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1}); ok {
		t.Fatal("expired authorization emitted ambient audio")
	}
	if detector.calls != 4 {
		t.Fatalf("wake detector saw %d ambient frames, want 4", detector.calls)
	}
}

func TestInFlightWindowDiscardsSpeakerAudioAndTail(t *testing.T) {
	detector := &fakeKeywordDetector{}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 1, Silence: 2}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close only releases the fake detector and has no result.
	segmenter.SetInFlightWindow(time.Now().Add(time.Minute), 2)
	segmenter.Suspend()

	in := make(chan []int16, 5)
	for range 3 {
		in <- sampleFrame(2000, 4)
	}
	segmenter.ResumeAfter(time.Hour) // the hardware tail is still in the input buffer
	in <- sampleFrame(2000, 4)
	in <- sampleFrame(0, 4)
	close(in)
	if _, ok := <-segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1}); ok {
		t.Fatal("speaker output was transcribed as an in-flight command")
	}
}

func TestKeywordSegmenterDiscardsNonWakeAudio(t *testing.T) {
	detector := &fakeKeywordDetector{}
	segmenter := NewKeyword(Params{FrameSize: 4, PreRoll: 2, Silence: 2, MaxUtterance: 10}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	in := make(chan []int16, 5)
	for range 5 {
		in <- sampleFrame(1200, 4)
	}
	close(in)
	utterances := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 16000, Channels: 1})
	if _, ok := <-utterances; ok {
		t.Fatal("non-wake audio produced an utterance")
	}
	if detector.calls != 5 {
		t.Fatalf("detector calls = %d, want 5", detector.calls)
	}
}

func TestKeywordSegmenterRetainsPreRollAndCommand(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 3}
	segmenter := NewKeyword(Params{FrameSize: 4, PreRoll: 2, Silence: 2, MaxUtterance: 10}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	in := make(chan []int16, 6)
	in <- sampleFrame(1200, 4)
	in <- sampleFrame(1300, 4)
	in <- sampleFrame(1400, 4)
	in <- sampleFrame(1500, 4)
	in <- sampleFrame(0, 4)
	in <- sampleFrame(0, 4)
	close(in)

	utterances := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 16000, Channels: 1})
	u, ok := <-utterances
	if !ok {
		t.Fatal("wake match produced no command utterance")
	}
	if !u.WakeMatched || u.Keyword != "HEY_VOICE" || u.Truncated {
		t.Fatalf("unexpected utterance metadata: %+v", u)
	}
	if got, want := len(u.PCM), 20; got != want {
		t.Fatalf("command samples = %d, want %d", got, want)
	}
	if detector.calls != 3 {
		t.Fatalf("detector received command frames: calls=%d, want 3", detector.calls)
	}
	if _, ok := <-utterances; ok {
		t.Fatal("one wake produced more than one utterance")
	}
}

func TestKeywordSegmenterBoundsContinuousAudioAfterWake(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 1}
	segmenter := NewKeyword(Params{FrameSize: 4, PreRoll: 1, Silence: 2, MaxUtterance: 4}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	in := make(chan []int16, 8)
	for range 8 {
		in <- sampleFrame(2000, 4)
	}
	close(in)
	utterances := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 16000, Channels: 1})
	u := <-utterances
	if !u.Truncated || len(u.PCM) != 16 {
		t.Fatalf("continuous command was not bounded: truncated=%v samples=%d", u.Truncated, len(u.PCM))
	}
	for range utterances {
	}
}

func TestKeywordSegmenterFailsClosedOnDetectorError(t *testing.T) {
	detector := &fakeKeywordDetector{err: errors.New("decoder failed")}
	segmenter := NewKeyword(Params{FrameSize: 4, PreRoll: 1, Silence: 2, MaxUtterance: 4}, detector, nil)
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	in := make(chan []int16, 1)
	in <- sampleFrame(2000, 4)
	close(in)
	if _, ok := <-segmenter.Run(context.Background(), in, audio.Format{SampleRate: 16000, Channels: 1}); ok {
		t.Fatal("detector error emitted audio")
	}
}

func TestStandaloneWakeAcknowledgesThenCapturesOneFollowUp(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 1}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, PreRoll: 2, MinSpeech: 1, Silence: 2, MaxUtterance: 10}, detector, nil)
	in := make(chan []int16, 8)
	acknowledged := make(chan struct{})
	var acknowledgements, restores int
	segmenter.SetFollowUp(20, func(context.Context) {
		acknowledgements++
		in <- sampleFrame(3000, 4) // speaker feedback buffered during playback
		close(acknowledged)
	}, func() { restores++ })
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	out := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
	in <- sampleFrame(2000, 4) // wake match
	in <- sampleFrame(0, 4)    // standalone-wake probe
	<-acknowledged
	waitDrained(t, in)
	in <- sampleFrame(3000, 4) // hardware feedback tail: must be discarded
	in <- sampleFrame(2000, 4) // follow-up command
	in <- sampleFrame(0, 4)
	in <- sampleFrame(0, 4)
	close(in)

	u, ok := <-out
	if !ok {
		t.Fatal("follow-up command was not emitted")
	}
	if !u.WakeMatched || !u.WakeAcknowledged || u.Keyword != "HEY_VOICE" {
		t.Fatalf("follow-up metadata = %+v", u)
	}
	for _, sample := range u.PCM {
		if sample == 3000 {
			t.Fatal("acknowledgement audio leaked into the follow-up command")
		}
	}
	if acknowledgements != 1 || restores != 0 {
		t.Fatalf("acknowledgements=%d restores=%d before session handling", acknowledgements, restores)
	}
	if _, ok := <-out; ok {
		t.Fatal("one standalone wake emitted more than one command")
	}
}

func TestStandaloneWakeTimesOutWithoutEmittingAmbientAudio(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 1}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, PreRoll: 2, MinSpeech: 1, Silence: 2, MaxUtterance: 10}, detector, nil)
	in := make(chan []int16, 8)
	acknowledged := make(chan struct{})
	var restores int
	segmenter.SetFollowUp(3, func(context.Context) {
		in <- sampleFrame(3000, 4)
		close(acknowledged)
	}, func() { restores++ })
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	out := segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
	in <- sampleFrame(2000, 4)
	in <- sampleFrame(0, 4)
	<-acknowledged
	waitDrained(t, in)
	in <- sampleFrame(3000, 4) // feedback tail
	for range 3 {
		in <- sampleFrame(0, 4)
	}
	close(in)

	if _, ok := <-out; ok {
		t.Fatal("follow-up timeout emitted ambient audio")
	}
	if restores != 1 {
		t.Fatalf("indicator restores = %d, want 1", restores)
	}
}

func TestStandaloneWakeCancellationRestoresIdleState(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 1}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, PreRoll: 1, MinSpeech: 1, Silence: 2, MaxUtterance: 10}, detector, nil)
	in := make(chan []int16, 8)
	acknowledged := make(chan struct{})
	var restores int
	segmenter.SetFollowUp(20, func(context.Context) {
		in <- sampleFrame(3000, 4)
		close(acknowledged)
	}, func() { restores++ })
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	ctx, cancel := context.WithCancel(context.Background())
	out := segmenter.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	in <- sampleFrame(2000, 4)
	in <- sampleFrame(0, 4)
	<-acknowledged
	waitDrained(t, in)
	cancel()
	if _, ok := <-out; ok {
		t.Fatal("cancelled follow-up emitted audio")
	}
	if restores != 1 {
		t.Fatalf("indicator restores = %d, want 1", restores)
	}
}

func TestInlineCommandDoesNotPlayFollowUpAcknowledgement(t *testing.T) {
	detector := &fakeKeywordDetector{matchAt: 1}
	segmenter := NewKeyword(Params{Threshold: .01, FrameSize: 4, PreRoll: 1, MinSpeech: 1, Silence: 2, MaxUtterance: 10}, detector, nil)
	var acknowledgements int
	segmenter.SetFollowUp(20, func(context.Context) { acknowledgements++ }, func() {})
	if err := segmenter.Start(); err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close() // discard: Close has no result and only releases fake detector state.

	in := make(chan []int16, 4)
	in <- sampleFrame(2000, 4) // wake match
	in <- sampleFrame(2000, 4) // speech continues: inline command
	in <- sampleFrame(0, 4)
	in <- sampleFrame(0, 4)
	close(in)
	u := <-segmenter.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
	if acknowledgements != 0 || u.WakeAcknowledged {
		t.Fatalf("inline command acknowledgements=%d metadata=%+v", acknowledgements, u)
	}
	if len(u.PCM) == 0 {
		t.Fatal("inline command audio was discarded")
	}
}

func waitDrained(t *testing.T, ch chan []int16) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(ch) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("feedback audio was not drained")
		}
		time.Sleep(time.Millisecond)
	}
}

func sampleFrame(sample int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = sample
	}
	return out
}

func TestQueuedConversationAudioCannotCrossPlaybackOrWindowGeneration(t *testing.T) {
	for _, boundary := range []string{"playback", "new window", "deadline"} {
		t.Run(boundary, func(t *testing.T) {
			k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2}, &fakeKeywordDetector{}, nil)
			if err := k.Start(); err != nil {
				t.Fatal(err)
			}
			defer k.Close() // discard: Keyword.Close has no return value.
			k.SetInFlightWindow(time.Now().Add(time.Minute), 1)
			in := make(chan []int16, 4)
			in <- sampleFrame(2000, 4)
			in <- sampleFrame(2000, 4)
			in <- sampleFrame(0, 4)
			in <- sampleFrame(0, 4)
			close(in)
			out := k.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1})
			u := <-out
			if _, open := <-out; open {
				t.Fatal("extra utterance")
			}
			if !k.Accepts(u) {
				t.Fatal("fresh final-budget turn was rejected")
			}
			switch boundary {
			case "playback":
				k.Suspend()
				k.ResumeAfter(0)
			case "new window":
				k.SetInFlightWindow(time.Now().Add(time.Minute), 2)
			case "deadline":
				// Keep the epoch unchanged to exercise the deadline check,
				// independently of window replacement invalidation.
				k.busyMu.Lock()
				k.busyUntil = time.Now().Add(-time.Second)
				k.busyMu.Unlock()
			}
			if k.Accepts(u) {
				t.Fatal("queued old audio regained cloud authorization")
			}
		})
	}
}

func TestKeywordDrainsCaptureBoundaryWhileSessionHasQueuedAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	k.SetInFlightWindow(time.Now().Add(time.Minute), 3)
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	// Do not receive output until three utterances have crossed the producer
	// boundary. Blocking on a second delivery would deadlock this handshake
	// while the session was synthesizing or playing its answer.
	for range 3 {
		for _, value := range []int16{2000, 2000, 0, 0} {
			sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(value, 4), false)
		}
	}
	close(in)
	u := <-out
	if _, open := <-out; open {
		t.Fatal("busy session accumulated a second stale utterance")
	}
	if k.CanThink() {
		t.Fatal("thinking resumed before queued speech was accepted")
	}
	if !k.Accepts(u) {
		t.Fatal("first queued utterance lost authorization")
	}
	if !k.CanThink() {
		t.Fatal("thinking remained paused after the queue drained")
	}
}

func TestThinkingWaitsForSpeechAndCaptureRearmsAfterTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2}, &fakeKeywordDetector{}, nil)
	k.SetCaptureBoundary(boundary)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	k.SetInFlightWindow(time.Now().Add(time.Minute), 2)
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(2000, 4), false)
	if k.CanThink() {
		t.Fatal("thinking may interrupt a speech candidate")
	}
	k.Suspend()
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	k.ResumeAfter(time.Hour)
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(3000, 4), false)
	k.ResumeAfter(0)
	for _, value := range []int16{2000, 2000, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(value, 4), false)
	}
	close(in)
	u := <-out
	if !k.Accepts(u) {
		t.Fatal("fresh speech after tail did not rearm")
	}
	for _, sample := range u.PCM {
		if sample == 3000 {
			t.Fatal("playback or tail leaked into the command")
		}
	}
	if _, open := <-out; open {
		t.Fatal("playback became an extra command")
	}
}

func TestThinkingDoesNotEraseStandaloneWakeFollowUpWhileOwnerPauses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundary := audio.NewCaptureBoundary()
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2}, &fakeKeywordDetector{matchAt: 1}, nil)
	k.SetCaptureBoundary(boundary)
	k.SetFollowUp(10, func(context.Context) {}, func() {})
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	in := make(chan []int16)
	out := k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1})
	sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(2000, 4), false)
	// Probe, feedback guard, then a quiet owner pause longer than Silence.
	for range 5 {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(0, 4), false)
	}
	if k.CanThink() {
		t.Fatal("thinking would reset the authorized standalone follow-up")
	}
	for i, value := range []int16{2000, 2000, 0, 0} {
		sendBoundaryFrame(t, ctx, in, boundary, sampleFrame(value, 4), i == 3)
	}
	close(in)
	u := <-out
	if !u.WakeAcknowledged || !k.Accepts(u) {
		t.Fatal("owner pause lost the standalone command")
	}
	if _, open := <-out; open {
		t.Fatal("extra command")
	}
}
