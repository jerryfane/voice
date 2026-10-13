package vad

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/wake"
)

// KeywordLogger records detector decisions without retaining audio.
type KeywordLogger interface {
	Printf(string, ...any)
}

// Keyword discards ambient audio until a local streaming detector reports a
// configured wake phrase. It then emits one bounded command utterance.
type Keyword struct {
	P        Params
	Detector wake.Detector
	Logger   KeywordLogger

	followUpFrames int
	acknowledge    func(context.Context)
	restore        func()
	started        bool
	// Set before Run, alongside the capture producer's boundary.
	captureBoundary *audio.CaptureBoundary

	busyMu       sync.Mutex
	busyUntil    time.Time
	suspendUntil atomic.Int64
	busyTurns    int
}

func NewKeyword(p Params, detector wake.Detector, logger KeywordLogger) *Keyword {
	return &Keyword{P: p, Detector: detector, Logger: logger}
}

// SetCaptureBoundary enables synchronous recovery decisions after each frame.
// Configure it before Run; the capture producer must wait for every acknowledgement.
func (k *Keyword) SetCaptureBoundary(boundary *audio.CaptureBoundary) {
	k.captureBoundary = boundary
}

// SetFollowUp enables a single unwaked command after a standalone keyword.
// acknowledge blocks until the earcon finishes; restore returns the indicator
// to idle when no follow-up arrives.
func (k *Keyword) SetFollowUp(frames int, acknowledge func(context.Context), restore func()) {
	k.followUpFrames = max(1, frames)
	k.acknowledge = acknowledge
	k.restore = restore
}

// SetInFlightWindow authorizes a bounded number of wake-free utterances after
// a locally detected wake. A zero deadline or turn count closes the window.
// The session must close it when its task finishes or fails.
func (k *Keyword) SetInFlightWindow(until time.Time, turns int) {
	k.busyMu.Lock()
	k.busyUntil, k.busyTurns = until, turns
	k.busyMu.Unlock()
}

func (k *Keyword) inFlight() bool {
	k.busyMu.Lock()
	defer k.busyMu.Unlock()
	return k.busyTurns > 0 && time.Now().Before(k.busyUntil)
}

func (k *Keyword) usedInFlightTurn() {
	k.busyMu.Lock()
	if k.busyTurns > 0 {
		k.busyTurns--
	}
	k.busyMu.Unlock()
}

// Suspend discards microphone frames while Voice speaks. ResumeAfter keeps
// discarding the hardware playback tail before wake detection resumes.
func (k *Keyword) Suspend() {
	k.suspendUntil.Store(1<<63 - 1)
}

func (k *Keyword) ResumeAfter(tail time.Duration) {
	k.suspendUntil.Store(time.Now().Add(tail).UnixNano())
}

func (k *Keyword) Name() string { return k.Detector.Name() }
func (k *Keyword) Available() (bool, string) {
	return k.Detector.Available()
}
func (k *Keyword) Start() error {
	if k.started {
		return nil
	}
	if err := k.Detector.Start(); err != nil {
		return err
	}
	k.started = true
	return nil
}
func (k *Keyword) Close() {
	k.Detector.Close() // discard: Detector.Close has no result; native destroy functions cannot report failure.
	k.started = false
}

func (k *Keyword) Run(ctx context.Context, in <-chan []int16, f audio.Format) <-chan Utterance {
	out := make(chan Utterance, 1)
	go func() {
		defer close(out)
		if !k.started {
			k.logf("stage=wake state=failed source=sherpa err=%q", "keyword detector was not started")
			return
		}

		p := k.P
		if p.FrameSize <= 0 {
			p.FrameSize = 320
		}
		if p.Silence <= 0 {
			p.Silence = 35
		}
		if p.MaxUtterance <= 0 {
			p.MaxUtterance = 300
		}
		if p.PreRoll < 0 {
			p.PreRoll = 0
		}
		probeFrames := min(p.Silence, max(1, durationFrames(100*time.Millisecond, p.FrameSize, f.SampleRate)))
		standaloneSilence := min(probeFrames, max(1, durationFrames(60*time.Millisecond, p.FrameSize, f.SampleRate)))
		feedbackTail := max(1, durationFrames(60*time.Millisecond, p.FrameSize, f.SampleRate))
		followPreRoll := min(p.PreRoll, probeFrames)
		minFollowSpeech := max(1, p.MinSpeech)

		const (
			armed = iota
			probing
			inlineCommand
			feedbackTailGuard
			followUpWaiting
			followUpCommand
			busyProbing
			busyCommand
		)
		state := armed
		pre := make([][]int16, 0, p.PreRoll)
		var (
			frames           [][]int16
			probe            [][]int16
			followCandidate  [][]int16
			noise            = 0.00015
			silent           int
			probeSilent      int
			tailLeft         int
			followWaited     int
			peak             float64
			followPeak       float64
			keyword          string
			detectedAt       time.Time
			wakeAcknowledged bool
		)

		restore := func() {
			if wakeAcknowledged && k.restore != nil {
				k.restore()
			}
			wakeAcknowledged = false
		}
		reset := func() {
			state = armed
			pre = pre[:0]
			frames = nil
			probe = nil
			followCandidate = nil
			silent = 0
			probeSilent = 0
			tailLeft = 0
			followWaited = 0
			peak = 0
			followPeak = 0
			keyword = ""
			detectedAt = time.Time{}
		}
		emit := func(truncated bool) bool {
			n := 0
			for _, frame := range frames {
				n += len(frame)
			}
			pcm := make([]int16, 0, n)
			for _, frame := range frames {
				pcm = append(pcm, frame...)
			}
			utterance := Utterance{
				PCM:              pcm,
				Format:           f,
				Peak:             peak,
				Truncated:        truncated,
				WakeMatched:      true,
				Keyword:          keyword,
				WakeAcknowledged: wakeAcknowledged,
			}
			wasBusy := state == busyCommand
			select {
			case out <- utterance:
				if wasBusy {
					k.usedInFlightTurn()
				}
				// The session now owns restoring acknowledged feedback.
				wakeAcknowledged = false
				reset()
				return true
			case <-ctx.Done():
				restore()
				return false
			}
		}
		threshold := func() float64 {
			if p.Threshold != 0 {
				return p.Threshold
			}
			return math.Max(0.00045, noise*3.2)
		}

		quiet := 0
		discontinue := func() bool {
			restore()
			reset()
			quiet = 0
			noise = 0.00015
			var err error
			if detector, ok := k.Detector.(wake.ResettableDetector); ok {
				err = detector.Reset()
			} else {
				// Legacy detectors can reset by restarting their stream.
				k.Detector.Close() // discard: Detector.Close has no return value.
				err = k.Detector.Start()
			}
			if err != nil {
				k.logf("stage=wake state=failed source=sherpa err=%q", err)
				return false
			}
			return true
		}
		pending := false
		acknowledge := func() bool {
			if !pending || k.captureBoundary == nil {
				return true
			}
			pending = false
			idle := quiet >= p.Silence && state == armed && !k.inFlight() &&
				time.Now().UnixNano() >= k.suspendUntil.Load()
			return k.captureBoundary.Acknowledge(ctx, idle)
		}
		defer acknowledge()
	frameLoop:
		for {
			if !acknowledge() {
				restore()
				return
			}
			select {
			case <-ctx.Done():
				restore()
				return
			case frame, ok := <-in:
				if !ok {
					if state == inlineCommand || state == followUpCommand || state == busyCommand && k.inFlight() {
						emit(false)
					} else {
						restore()
					}
					return
				}
				pending = k.captureBoundary != nil
				if len(frame) == 0 {
					if !discontinue() {
						return
					}
					continue
				}
				if time.Now().UnixNano() < k.suspendUntil.Load() {
					wakeAcknowledged = false
					reset()
					quiet = 0
					continue
				}
				if (state == busyProbing || state == busyCommand) && !k.inFlight() {
					wakeAcknowledged = false
					reset()
					quiet = 0
					continue
				}
				level := rms(frame)
				if level >= threshold() {
					quiet = 0
				} else if quiet < p.Silence {
					quiet++
				}

				switch state {
				case armed:
					if k.inFlight() {
						if level >= threshold() {
							followCandidate = append(followCandidate[:0], clone(frame))
							followPeak = level
							state = busyProbing
						}
						continue
					}
					if level < threshold() {
						noise = noise*0.98 + level*0.02
					}
					pre = append(pre, clone(frame))
					if len(pre) > p.PreRoll {
						pre = pre[1:]
					}
					started := time.Now()
					match, err := k.Detector.Accept(f.SampleRate, frame)
					if err != nil {
						quiet = 0
						k.logf("stage=wake state=failed source=sherpa err=%q", err)
						return
					}
					if match == "" {
						continue
					}
					keyword = match
					detectedAt = time.Now()
					state = probing
					frames = append(frames[:0], pre...)
					pre = nil
					peak = level
					k.logf("stage=wake state=detected source=sherpa keyword=%q decode_ms=%.3f", match, elapsedKeywordMS(started))

				case probing:
					probe = append(probe, clone(frame))
					if level > peak {
						peak = level
					}
					if level < threshold() {
						probeSilent++
					} else {
						probeSilent = 0
					}
					if len(probe) < probeFrames {
						continue
					}
					if probeSilent < standaloneSilence || k.acknowledge == nil {
						frames = append(frames, probe...)
						probe = nil
						silent = probeSilent
						state = inlineCommand
						continue
					}

					k.logf("stage=wake state=acknowledging source=sherpa keyword=%q latency_ms=%.3f", keyword, elapsedKeywordMS(detectedAt))
					wakeAcknowledged = true
					k.acknowledge(ctx)
					if ctx.Err() != nil {
						restore()
						return
					}
					// Capture runs while the earcon blocks. Drop only the
					// frames already buffered during playback, then a short
					// hardware tail, so the sound cannot become the command.
					for buffered := len(in); buffered > 0; buffered-- {
						// Boundary capture is unbuffered, so this loop only
						// applies to legacy capture without acknowledgements.
						bufferedFrame, open := <-in
						if !open {
							restore()
							return
						}
						if len(bufferedFrame) == 0 {
							if !discontinue() {
								return
							}
							continue frameLoop
						}
					}
					frames = nil
					probe = nil
					pre = make([][]int16, 0, followPreRoll)
					tailLeft = feedbackTail
					state = feedbackTailGuard

				case feedbackTailGuard:
					tailLeft--
					if tailLeft <= 0 {
						state = followUpWaiting
					}

				case followUpWaiting:
					followWaited++
					if level >= threshold() {
						followCandidate = append(followCandidate, clone(frame))
						if level > followPeak {
							followPeak = level
						}
						if len(followCandidate) >= minFollowSpeech {
							frames = append(frames[:0], pre...)
							frames = append(frames, followCandidate...)
							pre = nil
							followCandidate = nil
							peak = followPeak
							followPeak = 0
							silent = 0
							state = followUpCommand
						}
					} else {
						followCandidate = nil
						followPeak = 0
						noise = noise*0.98 + level*0.02
						pre = append(pre, clone(frame))
						if len(pre) > followPreRoll {
							pre = pre[1:]
						}
					}
					if state == followUpWaiting && followWaited >= k.followUpFrames {
						k.logf("stage=wake state=follow_up_timeout keyword=%q", keyword)
						restore()
						reset()
					}

				case busyProbing:
					if level < threshold() {
						reset()
						continue
					}
					followCandidate = append(followCandidate, clone(frame))
					if level > followPeak {
						followPeak = level
					}
					if len(followCandidate) >= minFollowSpeech {
						frames = append(frames[:0], followCandidate...)
						followCandidate = nil
						peak = followPeak
						followPeak = 0
						silent = 0
						keyword = "IN_FLIGHT"
						wakeAcknowledged = true
						state = busyCommand
					}

				case inlineCommand, followUpCommand, busyCommand:
					frames = append(frames, clone(frame))
					if level > peak {
						peak = level
					}
					if level >= threshold() {
						silent = 0
					} else {
						silent++
					}
					if len(frames) >= p.MaxUtterance {
						if !emit(true) {
							return
						}
					} else if silent >= p.Silence {
						if !emit(false) {
							return
						}
					}
				}
			}
		}
	}()
	return out
}

func (k *Keyword) logf(format string, args ...any) {
	if k.Logger != nil {
		k.Logger.Printf(format, args...)
	}
}

func elapsedKeywordMS(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

func durationFrames(d time.Duration, frameSize, sampleRate int) int {
	if frameSize <= 0 || sampleRate <= 0 {
		return 1
	}
	return int(math.Ceil(d.Seconds() * float64(sampleRate) / float64(frameSize)))
}

var _ ManagedSegmenter = (*Keyword)(nil)
