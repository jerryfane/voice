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
