package audio

import "testing"

func TestEarconNoneIsSilent(t *testing.T) {
	for _, name := range []string{"", "none", "NONE"} {
		pcm, err := Earcon(name, Default(), 1)
		if err != nil {
			t.Fatalf("Earcon(%q) error: %v", name, err)
		}
		if len(pcm) != 0 {
			t.Errorf("Earcon(%q) produced %d samples, want silence", name, len(pcm))
		}
	}
}

// A misspelled sound must be reported, not silently ignored: config validation
// depends on this to tell the user their acknowledgement sound will never play.
func TestEarconRejectsUnknownName(t *testing.T) {
	if _, err := Earcon("ding", Default(), 1); err == nil {
		t.Fatal("unknown sound accepted")
	}
}

func TestEarconIsShortAndScaledByVolume(t *testing.T) {
	f := Default()
	loud, err := Earcon(EarconChime, f, 1)
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := Earcon(EarconChime, f, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if ms := len(loud) * 1000 / f.SampleRate; ms < 20 || ms > 400 {
		t.Errorf("chime is %d ms long, want a short acknowledgement (20-400 ms)", ms)
	}
	if len(quiet) != len(loud) {
		t.Fatalf("volume changed the length: %d vs %d samples", len(quiet), len(loud))
	}
	if peak(quiet)*4 >= peak(loud) {
		t.Errorf("volume 0.1 peaked at %d, barely below volume 1.0 at %d", peak(quiet), peak(loud))
	}
	silent, err := Earcon(EarconChime, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(silent) != 0 {
		t.Errorf("volume 0 produced %d samples, want silence", len(silent))
	}
}

// The microphone is still capturing while the earcon plays, so the waveform
// must not start or end with a step that the speaker turns into a click.
func TestEarconStartsAndEndsNearZero(t *testing.T) {
	pcm, err := Earcon(EarconChime, Default(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) == 0 {
		t.Fatal("no samples")
	}
	const quiet = 1 << 10 // ~3% of full scale
	if abs(pcm[0]) > quiet || abs(pcm[len(pcm)-1]) > quiet {
		t.Errorf("waveform edges are %d and %d, want a ramp below %d", pcm[0], pcm[len(pcm)-1], quiet)
	}
}

func peak(pcm []int16) int {
	m := 0
	for _, s := range pcm {
		if v := abs(s); v > m {
			m = v
		}
	}
	return m
}

func abs(s int16) int {
	v := int(s)
	if v < 0 {
		return -v
	}
	return v
}
