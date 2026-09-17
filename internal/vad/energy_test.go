package vad

import (
	"context"
	"github.com/jerryfane/herdr-voice/internal/audio"
	"testing"
	"time"
)

func frame(v int16, n int) []int16 {
	x := make([]int16, n)
	for i := range x {
		x[i] = v
	}
	return x
}
func TestEnergyEmitsSpeechAndStopsAtSilence(t *testing.T) {
	in := make(chan []int16, 32)
	e := NewEnergy(Params{Threshold: .01, MinSpeech: 3, Silence: 3, MaxUtterance: 30, PreRoll: 2, FrameSize: 16})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out := e.Run(ctx, in, audio.Format{SampleRate: 800, Channels: 1})
	for range 3 {
		in <- frame(0, 16)
	}
	for range 5 {
		in <- frame(1000, 16)
	}
	for range 3 {
		in <- frame(0, 16)
	}
	close(in)
	u, ok := <-out
	if !ok {
		t.Fatal("no utterance")
	}
	if len(u.PCM) != (2+5+3)*16 {
		t.Fatalf("got %d samples", len(u.PCM))
	}
	if u.Truncated {
		t.Fatal("unexpected truncation")
	}
}
func TestEnergyRejectsShortClick(t *testing.T) {
	in := make(chan []int16, 8)
	e := NewEnergy(Params{Threshold: .01, MinSpeech: 3, Silence: 2, MaxUtterance: 20, FrameSize: 16})
	out := e.Run(context.Background(), in, audio.Format{SampleRate: 800, Channels: 1})
	in <- frame(1000, 16)
	in <- frame(0, 16)
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("short click emitted as speech")
	}
}
