package vad

import (
	"context"
	"github.com/jerryfane/voice/internal/audio"
	"testing"
	"time"
)

func TestCaptureDiscontinuityClosesWakeFreeAuthorization(t *testing.T) {
	k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 2, Silence: 2, MaxUtterance: 20}, &fakeKeywordDetector{}, nil)
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	defer k.Close() // discard: Keyword.Close has no return value.
	k.SetInFlightWindow(time.Now().Add(time.Minute), 2)
	in := make(chan []int16, 8)
	in <- sampleFrame(1000, 4)
	in <- nil
	for _, v := range []int16{1000, 1000, 1000, 0, 0} {
		in <- sampleFrame(v, 4)
	}
	close(in)
	for u := range k.Run(context.Background(), in, audio.Format{SampleRate: 40, Channels: 1}) {
		t.Errorf("unauthorized post-discontinuity utterance: %v", u.PCM)
	}
	if k.inFlight() {
		t.Fatal("capture loss retained wake-free authorization")
	}
	select {
	case <-k.Discontinuities():
	default:
		t.Fatal("session was not notified of capture loss")
	}
}
