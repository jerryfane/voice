package audio

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserveStreamPreservesPCMAndBoundaries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []int16, 2)
	frame := []int16{1, -3}
	input <- frame
	input <- nil
	close(input)
	var received, delivered atomic.Int32
	out := ObserveStream(ctx, input, func([]int16) { received.Add(1) }, func() { delivered.Add(1) })
	first := <-out
	if &first[0] != &frame[0] {
		t.Fatal("observer copied audio")
	}
	boundary, ok := <-out
	if !ok || boundary != nil {
		t.Fatal("discontinuity lost")
	}
	if _, ok := <-out; ok {
		t.Fatal("observer did not close")
	}
	if received.Load() != 2 || delivered.Load() != 1 {
		t.Fatalf("counts=%d/%d", received.Load(), delivered.Load())
	}
}

func TestObserveStreamReportsReceiptBeforeBlockedConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan []int16, 1)
	input <- []int16{1}
	observed := make(chan struct{})
	var delivered atomic.Bool
	out := ObserveStream(ctx, input, func([]int16) { close(observed) }, func() { delivered.Store(true) })
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("receipt blocked on consumer")
	}
	if delivered.Load() {
		t.Fatal("delivery invented for blocked consumer")
	}
	cancel()
	for range out {
	}
}
