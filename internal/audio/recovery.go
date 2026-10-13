package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// RecoveryOptions bounds capture health checks and retries. Zero PCM means
// exactly zero signed samples, not audio below a speech/volume threshold.
type RecoveryOptions struct {
	ReadyTimeout      time.Duration
	StallTimeout      time.Duration
	ZeroTimeout       time.Duration
	RetryInterval     time.Duration
	PreferredInterval time.Duration
	// ReturnQuiet and ReturnThreshold only gate an elective preferred return.
	// The active segmenter must independently acknowledge a safe boundary.
	ReturnQuiet     time.Duration
	ReturnThreshold float64
}

func DefaultRecoveryOptions() RecoveryOptions {
	return RecoveryOptions{
		ReadyTimeout: 5 * time.Second, StallTimeout: 3 * time.Second,
		ZeroTimeout: 10 * time.Second, RetryInterval: 2 * time.Second,
		PreferredInterval: 30 * time.Second, ReturnQuiet: 700 * time.Millisecond,
		ReturnThreshold: 0.00045,
	}
}

// CaptureChoice names a recorder and an optional exclusive hardware identity.
// Equal nonempty groups cannot be probed concurrently (notably shared HID).
type CaptureChoice struct {
	Recorder Recorder
	Group    string
}

// RecoveringRecorder keeps one logical PCM stream alive across device failures.
// Recoverable faults are logged, never sent as fatal stream errors. A nil frame
// announces a discontinuity; consumers must discard partial utterances. Without
// a CaptureBoundary it still recovers failures but never electively switches a
// healthy fallback: silence guessed outside the segmenter cannot prove safety.
type RecoveringRecorder struct {
	choices  []CaptureChoice
	options  RecoveryOptions
	boundary *CaptureBoundary
	logf     func(string, ...any)
	selected func(int)
}

func NewRecoveringRecorder(choices []CaptureChoice, options RecoveryOptions, boundary *CaptureBoundary, logf func(string, ...any), selected func(int)) (*RecoveringRecorder, error) {
	if len(choices) == 0 {
		return nil, errors.New("capture recovery requires at least one recorder")
	}
	for i, choice := range choices {
		if choice.Recorder == nil {
			return nil, fmt.Errorf("capture choice %d is nil", i)
		}
		if choice.Recorder.Format() != choices[0].Recorder.Format() {
			return nil, fmt.Errorf("capture choice %d changes logical PCM format", i)
		}
	}
	if choices[0].Recorder.Format().SampleRate <= 0 || choices[0].Recorder.Format().SampleRate > MaxSampleRate || choices[0].Recorder.Format().Channels != 1 {
		return nil, errors.New("capture recovery requires bounded positive-rate mono PCM")
	}
	if options.ReadyTimeout <= 0 || options.StallTimeout <= 0 || options.ZeroTimeout <= 0 || options.RetryInterval <= 0 || options.PreferredInterval <= 0 || options.ReturnQuiet <= 0 || options.ReturnThreshold <= 0 || math.IsNaN(options.ReturnThreshold) || math.IsInf(options.ReturnThreshold, 0) {
		return nil, errors.New("capture recovery thresholds must be positive and finite")
	}
	for _, duration := range []time.Duration{options.ReadyTimeout, options.StallTimeout, options.ZeroTimeout, options.RetryInterval, options.PreferredInterval, options.ReturnQuiet} {
		if duration > time.Hour {
			return nil, errors.New("capture recovery durations must not exceed one hour")
		}
	}
	return &RecoveringRecorder{choices: append([]CaptureChoice(nil), choices...), options: options, boundary: boundary, logf: logf, selected: selected}, nil
}

func (r *RecoveringRecorder) Format() Format { return r.choices[0].Recorder.Format() }
func (r *RecoveringRecorder) Describe() string {
	return fmt.Sprintf("recovering %s (%d ordered choices)", r.choices[0].Recorder.Describe(), len(r.choices))
}

func (r *RecoveringRecorder) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

type captureEvent struct {
	frame []int16
	quiet bool
	err   error
}

type captureAttempt struct {
	index   int
	cancel  context.CancelFunc
	events  chan captureEvent
	done    chan struct{}
	cleanup error // written before done closes
}

const captureStopTimeout = 2 * time.Second

func (r *RecoveringRecorder) start(ctx context.Context, index int, probe bool) *captureAttempt {
	ctx, cancel := context.WithCancel(ctx)
	a := &captureAttempt{index: index, cancel: cancel, events: make(chan captureEvent), done: make(chan struct{})}
	r.log("stage=capture state=starting choice=%d probe=%v device=%q", index, probe, r.choices[index].Recorder.Describe())
	pcm, errs := r.choices[index].Recorder.Stream(ctx)
	go func() {
		defer close(a.done)
		defer close(a.events)
		defer func() {
			cancel()
			// Recorder closure is its cleanup barrier. Drain both channels so
			// buffered error/frame delivery cannot block process/HID release.
			timer := time.NewTimer(captureStopTimeout)
			defer timer.Stop()
			for pcm != nil || errs != nil {
				select {
				case _, ok := <-pcm:
					if !ok {
						pcm = nil
					}
				case _, ok := <-errs:
					if !ok {
						errs = nil
					}
				case <-timer.C:
					a.cleanup = errors.New("capture did not close after cancellation; refusing overlapping retries")
					return
				}
			}
		}()
		send := func(event captureEvent) bool {
			select {
			case a.events <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		started, last := time.Now(), time.Now()
		ready := false
		var zeros, quiet int64
		var zeroSince time.Time
		rate := int64(r.Format().SampleRate)
		zeroLimit := max(int64(1), int64(r.options.ZeroTimeout)*rate/int64(time.Second))
		quietLimit := max(int64(1), int64(r.options.ReturnQuiet)*rate/int64(time.Second))
		timer := time.NewTimer(r.options.StallTimeout)
		defer timer.Stop()
		for {
			deadline := last.Add(r.options.StallTimeout)
			if !ready && started.Add(r.options.ReadyTimeout).Before(deadline) {
				deadline = started.Add(r.options.ReadyTimeout)
			}
			if !zeroSince.IsZero() && zeroSince.Add(r.options.ZeroTimeout).Before(deadline) {
				deadline = zeroSince.Add(r.options.ZeroTimeout)
			}
			timer.Reset(time.Until(deadline))
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				reason := "no PCM (capture stalled)"
				if !ready && !time.Now().Before(started.Add(r.options.ReadyTimeout)) {
					reason = "no nonzero PCM before readiness deadline"
				}
				if !zeroSince.IsZero() && !time.Now().Before(zeroSince.Add(r.options.ZeroTimeout)) {
					reason = "persistent exactly-zero PCM"
				}
				send(captureEvent{err: errors.New(reason)})
				return
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if err != nil {
					send(captureEvent{err: err})
					return
				}
			case frame, ok := <-pcm:
				if !ok {
					pcm = nil
					send(captureEvent{err: errors.New("capture PCM stream ended")})
					return
				}
				if len(frame) == 0 {
					continue // Empty buffers do not satisfy the PCM watchdog.
				}
				allZero := true
				var energy float64
				for _, sample := range frame {
					if sample != 0 {
						allZero = false
						if !probe {
							break
						}
					}
					if probe {
						v := float64(sample) / 32768
						energy += v * v
					}
				}
				if allZero {
					zeros += int64(len(frame))
					if zeroSince.IsZero() {
						zeroSince = time.Now()
					}
				} else {
					zeros = 0
					zeroSince = time.Time{}
				}
				if zeros >= zeroLimit {
					send(captureEvent{err: errors.New("persistent exactly-zero PCM")})
					return
				}
				if probe {
					if energy/float64(len(frame)) < r.options.ReturnThreshold*r.options.ReturnThreshold {
						quiet += int64(len(frame))
					} else {
						quiet = 0
					}
				}
				if !ready && !allZero {
					ready = true
					r.log("stage=capture state=healthy choice=%d probe=%v", index, probe)
				}
				if ready {
					blocked := time.Now()
					if !send(captureEvent{frame: frame, quiet: quiet >= quietLimit}) {
						return
					}
					if !zeroSince.IsZero() {
						zeroSince = zeroSince.Add(time.Since(blocked))
					}
				}
				// Downstream backpressure is not a broken microphone.
				last = time.Now()
			}
		}
	}()
	return a
}

func stopCapturePair(active, probe *captureAttempt) error {
	// Cancel both before waiting, so the cleanup bound does not multiply
	// when shutdown coincides with a preferred-input probe.
	if active != nil {
		active.cancel()
	}
	if probe != nil {
		probe.cancel()
	}
	return errors.Join(stopCapture(active), stopCapture(probe))
}

func stopCapture(a *captureAttempt) error {
	if a == nil {
		return nil
	}
	a.cancel()
	<-a.done // The attempt owns a bounded cancellation/cleanup barrier.
	return a.cleanup
}

func (r *RecoveringRecorder) Stream(ctx context.Context) (<-chan []int16, <-chan error) {
	out := make(chan []int16)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		var active, probe *captureAttempt
		defer func() {
			if err := stopCapturePair(active, probe); err != nil {
				r.log("stage=capture state=failed err=%q", err)
				if ctx.Err() == nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
		forward := func(frame []int16) (bool, bool) {
			select {
			case out <- frame:
			case <-ctx.Done():
				return false, false
			}
			if r.boundary == nil {
				return false, true
			}
			return r.boundary.Wait(ctx)
		}
		delay := func() bool {
			timer := time.NewTimer(r.options.RetryInterval)
			defer timer.Stop()
			select {
			case <-timer.C:
				return true
			case <-ctx.Done():
				return false
			}
		}
		active = r.start(ctx, 0, false)
		selected, delivered := false, false
		probeQuiet := false
		probeIndex := 0
		preferred := time.NewTimer(r.options.PreferredInterval)
		defer preferred.Stop()
		for {
			var probeEvents <-chan captureEvent
			if probe != nil {
				probeEvents = probe.events
			}
			select {
			case <-ctx.Done():
				return
			case <-preferred.C:
				if active.index > 0 && probe == nil && r.boundary != nil {
					group := r.choices[active.index].Group
					for probeIndex < active.index && group != "" && group == r.choices[probeIndex].Group {
						probeIndex++
					}
					if probeIndex < active.index {
						probe = r.start(ctx, probeIndex, true)
						probeQuiet = false
					} else {
						probeIndex = 0
					}
				}
				preferred.Reset(r.options.PreferredInterval)
			case event, ok := <-probeEvents:
				if !ok || event.err != nil {
					r.log("stage=capture state=retry choice=%d probe=true err=%q", probe.index, event.err)
					if err := stopCapture(probe); err != nil {
						errs <- err
						probe = nil
						return
					}
					probe = nil
					probeIndex++
					if probeIndex >= active.index {
						probeIndex = 0
					}
					preferred.Reset(r.options.PreferredInterval)
					continue
				}
				probeQuiet = event.quiet
			case event, ok := <-active.events:
				if !ok || event.err != nil {
					r.log("stage=capture state=recovering choice=%d err=%q", active.index, event.err)
					next := (active.index + 1) % len(r.choices)
					err := stopCapturePair(active, probe)
					active, probe = nil, nil
					if err != nil {
						errs <- err
						return
					}
					if delivered {
						if _, ok := forward(nil); !ok {
							return
						}
					}
					if !delay() {
						return
					}
					active = r.start(ctx, next, false)
					selected, probeQuiet = false, false
					probeIndex = 0
					preferred.Reset(r.options.PreferredInterval)
					continue
				}
				if !selected {
					if r.selected != nil {
						r.selected(active.index)
					}
					r.log("stage=capture state=ready choice=%d device=%q", active.index, r.choices[active.index].Recorder.Describe())
					selected = true
				}
				idle, open := forward(event.frame)
				if !open {
					return
				}
				delivered = true
				// Both microphones must have completed quiet, and the active
				// segmenter must have consumed this exact frame. A mere timer
				// or a stale atomic idle flag would cut a later utterance.
				if idle && probe != nil && probeQuiet {
					if err := stopCapture(active); err != nil {
						errs <- err
						active = nil
						return
					}
					active, probe = probe, nil
					selected, probeQuiet = false, false
					probeIndex = 0
					if _, ok := forward(nil); !ok {
						return
					}
					preferred.Reset(r.options.PreferredInterval)
				}
			}
		}
	}()
	return out, errs
}
