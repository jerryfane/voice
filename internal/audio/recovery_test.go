package audio

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type captureInput struct {
	pcm  []int16
	done chan struct{}
}

type fakeCapture struct {
	input   chan captureInput
	fail    chan error
	stopped chan struct{}
}

type recoveryRecorder struct {
	name    string
	started chan *fakeCapture
	live    atomic.Int32
	format  Format
}

func newRecoveryRecorder(name string) *recoveryRecorder {
	return &recoveryRecorder{name: name, started: make(chan *fakeCapture, 16), format: Default()}
}
func (r *recoveryRecorder) Format() Format   { return r.format }
func (r *recoveryRecorder) Describe() string { return r.name }
func (r *recoveryRecorder) Stream(ctx context.Context) (<-chan []int16, <-chan error) {
	pcm := make(chan []int16)
	errs := make(chan error, 1)
	c := &fakeCapture{input: make(chan captureInput), fail: make(chan error), stopped: make(chan struct{})}
	r.live.Add(1)
	r.started <- c
	go func() {
		defer close(pcm)
		defer close(errs)
		defer close(c.stopped)
		defer r.live.Add(-1)
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-c.fail:
				if err != nil {
					errs <- err
				}
				return
			case input := <-c.input:
				select {
				case pcm <- input.pcm:
					close(input.done)
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return pcm, errs
}

func recoveryOptions() RecoveryOptions {
	o := DefaultRecoveryOptions()
	o.ReadyTimeout = 300 * time.Millisecond
	o.StallTimeout = 300 * time.Millisecond
	o.ZeroTimeout = 60 * time.Millisecond
	o.RetryInterval = 5 * time.Millisecond
	o.PreferredInterval = 15 * time.Millisecond
	o.ReturnQuiet = 20 * time.Millisecond
	return o
}

func waitCapture(t *testing.T, r *recoveryRecorder) *fakeCapture {
	t.Helper()
	select {
	case c := <-r.started:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not start")
		return nil
	}
}
func sendCapture(t *testing.T, c *fakeCapture, frame []int16) {
	t.Helper()
	input := captureInput{pcm: frame, done: make(chan struct{})}
	select {
	case c.input <- input:
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not accept frame")
	}
	select {
	case <-input.done:
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not deliver frame")
	}
}
func pcmFrame(sample int16) []int16 {
	frame := make([]int16, 320)
	for i := range frame {
		frame[i] = sample
	}
	return frame
}
func nextPCM(t *testing.T, pcm <-chan []int16) []int16 {
	t.Helper()
	select {
	case frame, ok := <-pcm:
		if !ok {
			t.Fatal("logical stream closed")
		}
		return frame
	case <-time.After(3 * time.Second):
		t.Fatal("missing PCM")
		return nil
	}
}
func endRecovery(t *testing.T, cancel context.CancelFunc, pcm <-chan []int16, errs <-chan error, recorders ...*recoveryRecorder) {
	t.Helper()
	cancel()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for pcm != nil || errs != nil {
		select {
		case _, ok := <-pcm:
			if !ok {
				pcm = nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil {
				t.Errorf("unexpected fatal error: %v", err)
			}
		case <-timer.C:
			t.Fatal("capture shutdown was not bounded")
		}
	}
	for _, r := range recorders {
		if n := r.live.Load(); n != 0 {
			t.Errorf("%s has %d live attempts", r.name, n)
		}
	}
}
func makeRecovery(t *testing.T, recorders []*recoveryRecorder, options RecoveryOptions, boundary *CaptureBoundary, selected func(int)) *RecoveringRecorder {
	t.Helper()
	choices := make([]CaptureChoice, len(recorders))
	for i, recorder := range recorders {
		choices[i] = CaptureChoice{Recorder: recorder}
	}
	r, err := NewRecoveringRecorder(choices, options, boundary, nil, selected)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecoveryReadinessRequiresSamplesButAcceptsQuietNonzero(t *testing.T) {
	source := newRecoveryRecorder("quiet")
	selected := make(chan int, 1)
	r := makeRecovery(t, []*recoveryRecorder{source}, recoveryOptions(), nil, func(i int) { selected <- i })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := r.Stream(ctx)
	capture := waitCapture(t, source)
	sendCapture(t, capture, nil)
	sendCapture(t, capture, pcmFrame(0))
	select {
	case <-selected:
		t.Fatal("empty/digital-zero capture claimed readiness")
	default:
	}
	quiet := pcmFrame(1)
	sendCapture(t, capture, quiet)
	if got := nextPCM(t, pcm); !slices.Equal(got, quiet) {
		t.Fatal("first healthy samples changed")
	}
	select {
	case i := <-selected:
		if i != 0 {
			t.Fatalf("selected %d", i)
		}
	case <-time.After(time.Second):
		t.Fatal("no readiness")
	}
	for range 5 {
		sendCapture(t, capture, quiet)
		nextPCM(t, pcm)
	}
	select {
	case <-capture.stopped:
		t.Fatal("quiet nonzero input was treated as digital silence")
	default:
	}
	endRecovery(t, cancel, pcm, errs, source)
}

func TestRecoveryRetriesChoicesInOrderAndKeepsLogicalStream(t *testing.T) {
	recorders := []*recoveryRecorder{newRecoveryRecorder("preferred"), newRecoveryRecorder("second"), newRecoveryRecorder("third")}
	selected := make(chan int, 4)
	r := makeRecovery(t, recorders, recoveryOptions(), nil, func(i int) { selected <- i })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := r.Stream(ctx)
	first := waitCapture(t, recorders[0])
	first.fail <- errors.New("unplugged")
	second := waitCapture(t, recorders[1])
	second.fail <- errors.New("unavailable")
	third := waitCapture(t, recorders[2])
	sendCapture(t, third, pcmFrame(3))
	if got := nextPCM(t, pcm)[0]; got != 3 {
		t.Fatalf("fallback PCM = %d", got)
	}
	if i := <-selected; i != 2 {
		t.Fatalf("fallback index = %d", i)
	}
	third.fail <- errors.New("lost fallback")
	if got := nextPCM(t, pcm); got != nil {
		t.Fatal("failure did not delimit sources")
	}
	first = waitCapture(t, recorders[0])
	sendCapture(t, first, pcmFrame(4))
	if got := nextPCM(t, pcm)[0]; got != 4 {
		t.Fatalf("recovered PCM = %d", got)
	}
	if i := <-selected; i != 0 {
		t.Fatalf("preferred index = %d", i)
	}
	endRecovery(t, cancel, pcm, errs, recorders...)
}

func TestRecoveryDetectsStallAndPersistentDigitalZeros(t *testing.T) {
	for _, mode := range []string{"stall", "zero", "empty", "exit"} {
		t.Run(mode, func(t *testing.T) {
			primary, fallback := newRecoveryRecorder("primary"), newRecoveryRecorder("fallback")
			o := recoveryOptions()
			o.StallTimeout = 40 * time.Millisecond
			r := makeRecovery(t, []*recoveryRecorder{primary, fallback}, o, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pcm, errs := r.Stream(ctx)
			capture := waitCapture(t, primary)
			sendCapture(t, capture, pcmFrame(5))
			nextPCM(t, pcm)
			switch mode {
			case "zero":
				for range 2 {
					sendCapture(t, capture, pcmFrame(0))
					nextPCM(t, pcm)
				}
				sendCapture(t, capture, pcmFrame(0))
			case "empty":
				sendCapture(t, capture, nil)
			case "exit":
				capture.fail <- nil
			}
			if nextPCM(t, pcm) != nil {
				t.Fatal("missing discontinuity")
			}
			capture = waitCapture(t, fallback)
			sendCapture(t, capture, pcmFrame(9))
			if nextPCM(t, pcm)[0] != 9 {
				t.Fatal("fallback did not resume")
			}
			endRecovery(t, cancel, pcm, errs, primary, fallback)
		})
	}
}

func TestRecoveryPreferredReturnWaitsForSafeBoundary(t *testing.T) {
	primary, fallback := newRecoveryRecorder("primary"), newRecoveryRecorder("fallback")
	boundary := NewCaptureBoundary()
	selected := make(chan int, 4)
	r := makeRecovery(t, []*recoveryRecorder{primary, fallback}, recoveryOptions(), boundary, func(i int) { selected <- i })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := r.Stream(ctx)
	waitCapture(t, primary).fail <- errors.New("missing")
	active := waitCapture(t, fallback)
	sendCapture(t, active, pcmFrame(1000))
	nextPCM(t, pcm)
	boundary.Acknowledge(ctx, false)
	if <-selected != 1 {
		t.Fatal("fallback not selected")
	}
	probe := waitCapture(t, primary)
	// Delivery of the second frame proves the first quiet event was consumed.
	sendCapture(t, probe, pcmFrame(1))
	sendCapture(t, probe, pcmFrame(1))
	sendCapture(t, active, pcmFrame(2000))
	nextPCM(t, pcm)
	boundary.Acknowledge(ctx, false)
	select {
	case <-active.stopped:
		t.Fatal("preferred return chopped an utterance")
	default:
	}
	sendCapture(t, active, pcmFrame(1))
	nextPCM(t, pcm)
	boundary.Acknowledge(ctx, true)
	if nextPCM(t, pcm) != nil {
		t.Fatal("preferred return did not reset segmentation")
	}
	boundary.Acknowledge(ctx, false)
	sendCapture(t, probe, pcmFrame(7))
	// One already-observed probe frame may be queued at the handoff, but no
	// frame from an active fallback utterance may be substituted or lost.
	frame := nextPCM(t, pcm)
	boundary.Acknowledge(ctx, false)
	if frame[0] != 1 && frame[0] != 7 {
		t.Fatalf("wrong preferred PCM %d", frame[0])
	}
	if <-selected != 0 {
		t.Fatal("preferred input not restored")
	}
	endRecovery(t, cancel, pcm, errs, primary, fallback)
}

func TestRecoveryCancellationInterruptsRetryAndBlockedDelivery(t *testing.T) {
	for _, mode := range []string{"retry", "delivery", "ack", "probe"} {
		t.Run(mode, func(t *testing.T) {
			primary, fallback := newRecoveryRecorder("primary"), newRecoveryRecorder("fallback")
			o := recoveryOptions()
			o.RetryInterval = 20 * time.Millisecond
			var boundary *CaptureBoundary
			if mode == "ack" || mode == "probe" {
				boundary = NewCaptureBoundary()
			}
			r := makeRecovery(t, []*recoveryRecorder{primary, fallback}, o, boundary, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pcm, errs := r.Stream(ctx)
			capture := waitCapture(t, primary)
			switch mode {
			case "retry":
				capture.fail <- errors.New("failed")
			case "delivery":
				sendCapture(t, capture, pcmFrame(1))
			case "ack":
				sendCapture(t, capture, pcmFrame(1))
				nextPCM(t, pcm)
			case "probe":
				capture.fail <- errors.New("failed")
				capture = waitCapture(t, fallback)
				sendCapture(t, capture, pcmFrame(1))
				nextPCM(t, pcm)
				boundary.Acknowledge(ctx, false)
				waitCapture(t, primary)
			}
			endRecovery(t, cancel, pcm, errs, primary, fallback)
		})
	}
}

func TestRecoveryRejectsFormatChange(t *testing.T) {
	first, second := newRecoveryRecorder("first"), newRecoveryRecorder("second")
	second.format.SampleRate = 48000
	_, err := NewRecoveringRecorder([]CaptureChoice{{Recorder: first}, {Recorder: second}}, recoveryOptions(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("format mismatch error = %v", err)
	}
}

func TestRecoveryAllUnavailableRetriesArePacedAndDoNotLeak(t *testing.T) {
	first, second := newRecoveryRecorder("first"), newRecoveryRecorder("second")
	o := recoveryOptions()
	o.RetryInterval = 20 * time.Millisecond
	r := makeRecovery(t, []*recoveryRecorder{first, second}, o, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := r.Stream(ctx)
	capture := waitCapture(t, first)
	for index := range 8 {
		started := time.Now()
		capture.fail <- errors.New("unavailable")
		next := second
		if index%2 != 0 {
			next = first
		}
		capture = waitCapture(t, next)
		if elapsed := time.Since(started); elapsed < o.RetryInterval {
			t.Fatalf("retry busy-loop: only %s between attempts", elapsed)
		}
		if first.live.Load()+second.live.Load() != 1 {
			t.Fatal("failed attempt remained live")
		}
	}
	endRecovery(t, cancel, pcm, errs, first, second)
}

func TestRecoveryNeverProbesSharedHardware(t *testing.T) {
	first, second := newRecoveryRecorder("first"), newRecoveryRecorder("second")
	boundary := NewCaptureBoundary()
	o := recoveryOptions()
	r, err := NewRecoveringRecorder([]CaptureChoice{{Recorder: first, Group: "shared-hid"}, {Recorder: second, Group: "shared-hid"}}, o, boundary, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := r.Stream(ctx)
	waitCapture(t, first).fail <- errors.New("failed")
	capture := waitCapture(t, second)
	until := time.NewTimer(5 * o.PreferredInterval)
	defer until.Stop()
	ticks := time.NewTicker(5 * time.Millisecond)
	defer ticks.Stop()
	running := true
	for running {
		select {
		case <-until.C:
			running = false
		case <-ticks.C:
			sendCapture(t, capture, pcmFrame(1))
			if nextPCM(t, pcm) == nil {
				t.Fatal("healthy shared-device fallback switched")
			}
			boundary.Acknowledge(ctx, true)
		case <-first.started:
			t.Fatal("preferred probe overlapped a shared HID capture")
		}
	}
	endRecovery(t, cancel, pcm, errs, first, second)
}
