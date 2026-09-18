package audio

import (
	"fmt"
	"math"
	"strings"
)

// Earcons are the acknowledgement sounds Voice can play when it accepts a wake
// phrase. They are generated at runtime instead of shipped as assets, so there
// is no licensing question and no file to install.
const (
	EarconNone  = "none"
	EarconChime = "chime"
	EarconBlip  = "blip"
	EarconTwoUp = "two-up"
)

// Thinking sounds are played on a loop while the planner is working, because
// a request that leaves the device takes seconds and silence during that wait
// is indistinguishable from Voice having missed the question. Measured on the
// owner's hardware: 7.102 s in the planner, during which nothing happened that
// a person in the room could perceive.
const (
	ThinkingNone = "none"
	// ThinkingTick is a soft low pulse: quiet, periodic, and clearly not an
	// answer, so nobody mistakes it for a reply.
	ThinkingTick = "tick"
	// ThinkingHum is a gentler two-note murmur for people who find a pulse
	// insistent.
	ThinkingHum = "hum"
)

// ThinkingVolumeRatio is the share of feedback.volume used for the thinking
// sound when feedback.thinking_volume is not set. It plays underneath someone
// waiting rather than at them.
const ThinkingVolumeRatio = 0.35

// Thinkings lists the selectable thinking sounds, "none" included.
func Thinkings() []string { return []string{ThinkingNone, ThinkingTick, ThinkingHum} }

// Thinking renders one iteration of a looping thinking sound, INCLUDING its
// trailing silence, so a caller can play it repeatedly without timing gaps
// itself. A cycle is about a second: long enough not to nag, short enough that
// stopping it lands close to the moment the answer arrives.
func Thinking(name string, f Format, volume float64) ([]int16, error) {
	// volume is the thinking sound's OWN level, already resolved by the
	// caller: see config.Feedback.ResolvedThinkingVolume, which derives it
	// from feedback.volume when feedback.thinking_volume is unset so an
	// existing config keeps the level it had.
	volume = math.Max(0, math.Min(1, volume))
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", ThinkingNone:
		return nil, nil
	case ThinkingTick:
		return render([]note{{392, 0.05, 0.7}, {0, 0.95, 0}}, f, volume), nil
	case ThinkingHum:
		return render([]note{{329.63, 0.09, 0.6}, {392, 0.09, 0.5}, {0, 0.82, 0}}, f, volume), nil
	default:
		return nil, fmt.Errorf("unknown thinking sound %q (choose one of %s)", name, strings.Join(Thinkings(), ", "))
	}
}

// Earcons lists the selectable sound names, "none" included.
func Earcons() []string { return []string{EarconNone, EarconChime, EarconBlip, EarconTwoUp} }

type note struct {
	freq float64 // Hz
	dur  float64 // seconds
	gain float64 // relative to volume
}

// Earcon renders a named acknowledgement sound. volume scales full scale and is
// clamped to [0,1]; name "none" (or empty) returns nil, which callers treat as
// "sound disabled". An unknown name is an error so a config typo is reported at
// startup instead of silently playing nothing.
func Earcon(name string, f Format, volume float64) ([]int16, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", EarconNone:
		return nil, nil
	case EarconChime:
		// Soft major third: recognisable, short, no startle.
		return render([]note{{880, 0.07, 1}, {1108.73, 0.11, 0.85}}, f, volume), nil
	case EarconBlip:
		return render([]note{{1046.5, 0.06, 1}}, f, volume), nil
	case EarconTwoUp:
		return render([]note{{659.26, 0.06, 0.9}, {987.77, 0.09, 1}}, f, volume), nil
	default:
		return nil, fmt.Errorf("unknown sound %q (choose one of %s)", name, strings.Join(Earcons(), ", "))
	}
}

// render concatenates notes into one mono buffer. Each note gets a raised-cosine
// attack and release so the speaker never clicks, which matters because the
// microphone is still capturing while the earcon plays.
func render(notes []note, f Format, volume float64) []int16 {
	rate := f.SampleRate
	if rate <= 0 {
		rate = Default().SampleRate
	}
	volume = math.Max(0, math.Min(1, volume))
	if volume == 0 {
		return nil
	}
	total := 0
	for _, n := range notes {
		total += int(n.dur * float64(rate))
	}
	out := make([]int16, 0, total)
	for _, n := range notes {
		samples := int(n.dur * float64(rate))
		if samples <= 0 {
			continue
		}
		ramp := int(0.008 * float64(rate)) // 8 ms
		if ramp*2 > samples {
			ramp = samples / 2
		}
		amp := volume * n.gain * 0.9 * math.MaxInt16
		step := 2 * math.Pi * n.freq / float64(rate)
		for i := range samples {
			env := 1.0
			if ramp > 0 {
				if i < ramp {
					env = 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(ramp))
				} else if rem := samples - 1 - i; rem < ramp {
					env = 0.5 - 0.5*math.Cos(math.Pi*float64(rem)/float64(ramp))
				}
			}
			out = append(out, int16(amp*env*math.Sin(step*float64(i))))
		}
	}
	return out
}
