package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/config"
)

// The owner recorded his own thinking sound. Validation accepting the file is
// not enough: the SESSION has to actually load it, and a wiring mistake there
// plays the built-in tone instead - his file silently ignored, with nothing
// failing. That is the defect this asserts against.
func TestBuildLoadsTheThinkingSoundFromAFile(t *testing.T) {
	dir := t.TempDir()
	// Distinctive length, so the built-in tone cannot be mistaken for it:
	// the generated cycle is about a second, this is a quarter of that.
	const samples = 16000 / 4
	pcm := make([]int16, samples)
	for i := range pcm {
		pcm[i] = 9000
	}
	path := filepath.Join(dir, "thinking.wav")
	if err := os.WriteFile(path, audio.EncodeWAV(pcm, audio.Format{SampleRate: 16000, Channels: 1}), 0o644); err != nil {
		t.Fatal(err)
	}

	c := config.Default()
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.Detector = "sherpa"
	c.Input.SampleRate = 16000
	c.Feedback.Thinking = path

	a := Build(c)
	if a.Feedback == nil {
		t.Fatal("no feedback notifier was built")
	}
	lead := int(playbackLeadIn * 16000 / time.Second)
	if got := len(a.Feedback.Working); got != samples+lead {
		t.Fatalf("thinking audio is %d samples, want %d from the file plus %d speaker wake-up samples", got, samples, lead)
	}
	for i, sample := range a.Feedback.Working {
		if i < lead && sample != 0 || i >= lead && sample == 0 {
			t.Fatalf("thinking audio sample %d = %d: the file or speaker wake-up padding was lost", i, sample)
		}
	}
	if a.Feedback.ThinkingGap < 2*playbackTail {
		t.Fatal("a custom sound without trailing silence leaves no listening gap")
	}
}
