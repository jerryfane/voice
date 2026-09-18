package vad

import (
	"context"
	"errors"
	"testing"

	"github.com/jerryfane/voice/internal/audio"
)

type fakeKeywordDetector struct {
	matchAt int
	calls   int
	started bool
	closed  bool
	err     error
}

func (*fakeKeywordDetector) Name() string              { return "fake keyword detector" }
func (*fakeKeywordDetector) Available() (bool, string) { return true, "available" }
func (d *fakeKeywordDetector) Start() error            { d.started = true; return nil }
func (d *fakeKeywordDetector) Close()                  { d.closed = true }
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

func sampleFrame(sample int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = sample
	}
	return out
}
