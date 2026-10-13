package vad

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

func TestFollowUpPreservesShortWordsAcrossUnvoicedFrames(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		name := "conversation"
		if standalone {
			name = "standalone wake"
		}
		t.Run(name, func(t *testing.T) {
			detector := &fakeKeywordDetector{}
			if standalone {
				detector.matchAt = 1
			}
			k := NewKeyword(Params{Threshold: .01, FrameSize: 4, MinSpeech: 3, Silence: 3, MaxUtterance: 20}, detector, nil)
			if err := k.Start(); err != nil {
				t.Fatal(err)
			}
			defer k.Close() // discard: Keyword.Close has no return value.
			k.SetFollowUp(30, func(context.Context) {}, func() {})
			if !standalone {
				k.SetInFlightWindow(time.Now().Add(time.Minute), 1)
			}
			in := make(chan []int16)
			var input [][]int16
			if standalone {
				input = append(input, sampleFrame(1000, 4), sampleFrame(0, 4), sampleFrame(0, 4))
			}
			// Three short voiced words separated by unvoiced frames: no word
			// individually meets MinSpeech, but the utterance does. Previously
			// every quiet frame discarded the preceding word ("What is the
			// date?" was physically transcribed as only "Tate.").
			var want []int16
			for _, level := range []int16{1000, 0, 2000, 0, 3000, 0, 0, 0} {
				frame := sampleFrame(level, 4)
				want = append(want, frame...)
				input = append(input, frame)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go func() {
				defer close(in)
				for _, frame := range input {
					select {
					case in <- frame:
					case <-ctx.Done():
						return
					}
				}
			}()
			count := 0
			for u := range k.Run(ctx, in, audio.Format{SampleRate: 40, Channels: 1}) {
				count++
				if !slices.Equal(u.PCM, want) {
					t.Fatalf("short-word onset lost: got %v want %v", u.PCM, want)
				}
			}
			if count != 1 {
				t.Fatalf("got %d utterances, want one complete utterance", count)
			}
		})
	}
}
