package session

import (
	"os"
	"path/filepath"
	"testing"

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
	c.Input.SampleRate = 16000
	c.Feedback.Thinking = path

	a := Build(c)
	if a.Feedback == nil {
		t.Fatal("no feedback notifier was built")
	}
	if got := len(a.Feedback.Working); got != samples {
		t.Errorf("thinking audio is %d samples, want %d from the file: the built-in tone was rendered instead of the owner's sound", got, samples)
	}
}
