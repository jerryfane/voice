package vad

import (
	"context"
	"math"

	"github.com/jerryfane/herdr-voice/internal/audio"
)

// Energy is a low-cost RMS gate with ambient-noise adaptation and hysteresis.
type Energy struct{ P Params }

func NewEnergy(p Params) *Energy {
	if p.FrameSize <= 0 {
		p.FrameSize = 320
	}
	if p.MinSpeech <= 0 {
		p.MinSpeech = 12
	}
	if p.Silence <= 0 {
		p.Silence = 35
	}
	if p.MaxUtterance <= 0 {
		p.MaxUtterance = 600
	}
	if p.PreRoll < 0 {
		p.PreRoll = 0
	}
	return &Energy{P: p}
}

func rms(x []int16) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, s := range x {
		v := float64(s) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

func clone(x []int16) []int16 { y := make([]int16, len(x)); copy(y, x); return y }

func (e *Energy) Run(ctx context.Context, in <-chan []int16, f audio.Format) <-chan Utterance {
	out := make(chan Utterance, 4)
	go func() {
		defer close(out)
		p := e.P
		pre := make([][]int16, 0, p.PreRoll)
		noise := 0.00015
		active, candidate := false, false
		speech, silent := 0, 0
		frames := make([][]int16, 0, p.MaxUtterance)
		peak := 0.0
		emit := func(truncated bool) {
			if !active || speech < p.MinSpeech {
				frames = nil
				active = false
				candidate = false
				speech = 0
				silent = 0
				peak = 0
				return
			}
			n := 0
			for _, fr := range frames {
				n += len(fr)
			}
			pcm := make([]int16, 0, n)
			for _, fr := range frames {
				pcm = append(pcm, fr...)
			}
			select {
			case out <- Utterance{PCM: pcm, Format: f, Peak: peak, Truncated: truncated}:
			case <-ctx.Done():
			}
			frames = nil
			active = false
			candidate = false
			speech = 0
			silent = 0
			peak = 0
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
				level := rms(frame)
				if level > peak {
					peak = level
				}
				threshold := p.Threshold
				if threshold == 0 {
					threshold = math.Max(0.00045, noise*3.2)
				}
				isSpeech := level >= threshold
				if !active {
					if !isSpeech {
						noise = noise*0.98 + level*0.02
					}
					if isSpeech {
						if !candidate {
							candidate = true
							frames = append(frames[:0], pre...)
							speech = 0
						}
						frames = append(frames, clone(frame))
						speech++
						if speech >= p.MinSpeech {
							active = true
						}
					} else if candidate {
						frames = append(frames, clone(frame))
						candidate = false
						speech = 0
						frames = nil
					}
					pre = append(pre, clone(frame))
					if len(pre) > p.PreRoll {
						pre = pre[1:]
					}
					continue
				}
				frames = append(frames, clone(frame))
				if isSpeech {
					speech++
					silent = 0
				} else {
					silent++
				}
				if len(frames) >= p.MaxUtterance {
					emit(true)
					pre = nil
				} else if silent >= p.Silence {
					emit(false)
					pre = nil
				}
			}
		}
	}()
	return out
}
