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

// A thinking sound must be quiet, must include its own trailing silence so a
// caller can loop it without timing gaps, and must not be mistaken for an
// answer. It exists because 7.1 seconds of planner time sounded exactly like
// Voice having missed the question.
func TestThinkingRendersAQuietLoopableCycle(t *testing.T) {
	f := Format{SampleRate: 16000, Channels: 1}
	for _, name := range []string{ThinkingTick, ThinkingHum} {
		pcm, err := Thinking(name, f, 1)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(pcm) == 0 {
			t.Fatalf("%s rendered nothing", name)
		}
		// About a second, so stopping the loop lands near the answer.
		secs := float64(len(pcm)) / float64(f.SampleRate)
		if secs < 0.8 || secs > 1.3 {
			t.Errorf("%s cycle is %.2fs, want roughly one second", name, secs)
		}
		// It ends in silence, which is what makes it loopable without a gap.
		tail := pcm[len(pcm)-f.SampleRate/10:]
		for _, s := range tail {
			if s != 0 {
				t.Errorf("%s does not end in silence, so looping it will stutter", name)
				break
			}
		}
		// Quieter than an acknowledgement at the same volume: it plays under
		// someone waiting, not at them.
		ack, err := Earcon(EarconChime, f, 1)
		if err != nil {
			t.Fatal(err)
		}
		if peak(pcm) >= peak(ack) {
			t.Errorf("%s peaks at %d, not below the acknowledgement's %d", name, peak(pcm), peak(ack))
		}
	}

	if pcm, err := Thinking(ThinkingNone, f, 1); err != nil || pcm != nil {
		t.Errorf("none must render nothing: %v %v", len(pcm), err)
	}
	if _, err := Thinking("wobble", f, 1); err == nil {
		t.Error("an unknown thinking sound must be an error, not silence")
	}
}
