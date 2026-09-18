package vad

import (
	"context"
	"math"
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
	started  bool
}

func NewKeyword(p Params, detector wake.Detector, logger KeywordLogger) *Keyword {
	return &Keyword{P: p, Detector: detector, Logger: logger}
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

		pre := make([][]int16, 0, p.PreRoll)
		var frames [][]int16
		noise := 0.00015
		active := false
		silent := 0
		peak := 0.0
		keyword := ""

		emit := func(truncated bool) bool {
			if !active {
				return true
			}
			n := 0
			for _, frame := range frames {
				n += len(frame)
			}
			pcm := make([]int16, 0, n)
			for _, frame := range frames {
				pcm = append(pcm, frame...)
			}
			utterance := Utterance{
				PCM:         pcm,
				Format:      f,
				Peak:        peak,
				Truncated:   truncated,
				WakeMatched: true,
				Keyword:     keyword,
			}
			select {
			case out <- utterance:
			case <-ctx.Done():
				return false
			}
			frames = nil
			pre = nil
			active = false
			silent = 0
			peak = 0
			keyword = ""
			return true
		}

		for {
			select {
			case <-ctx.Done():
				emit(false)
				return
			case frame, ok := <-in:
				if !ok {
					emit(false)
					return
				}
				if len(frame) == 0 {
					continue
				}
				level := rms(frame)

				if !active {
					threshold := p.Threshold
					if threshold == 0 {
						threshold = math.Max(0.00045, noise*3.2)
					}
					if level < threshold {
						noise = noise*0.98 + level*0.02
					}
					pre = append(pre, clone(frame))
					if len(pre) > p.PreRoll {
						pre = pre[1:]
					}
					started := time.Now()
					match, err := k.Detector.Accept(f.SampleRate, frame)
					if err != nil {
						k.logf("stage=wake state=failed source=sherpa err=%q", err)
						return
					}
					if match == "" {
						continue
					}
					keyword = match
					active = true
					frames = pre
					pre = nil
					peak = level
					k.logf("stage=wake state=detected source=sherpa keyword=%q decode_ms=%.3f", match, elapsedKeywordMS(started))
					continue
				}

				frames = append(frames, clone(frame))
				if level > peak {
					peak = level
				}
				threshold := p.Threshold
				if threshold == 0 {
					threshold = math.Max(0.00045, noise*3.2)
				}
				if level >= threshold {
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

var _ ManagedSegmenter = (*Keyword)(nil)
