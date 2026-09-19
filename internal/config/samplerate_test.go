package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/audio"
)

// Review panicked the REAL startup path with input.sample_rate=MaxInt. The
// config must refuse it long before any audio buffer is sized from it.
func TestInputSampleRateIsBounded(t *testing.T) {
	for _, rate := range []int{1 << 62, 1 << 40, 3999, 192001, 0, -1} {
		c := Default()
		c.Input.SampleRate = rate
		if err := c.Validate(); err == nil {
			t.Errorf("input.sample_rate %d was accepted; it sizes every audio buffer", rate)
		}
	}
	for _, rate := range []int{8000, 16000, 44100, 48000} {
		c := Default()
		c.Input.SampleRate = rate
		if err := c.Validate(); err != nil {
			t.Errorf("ordinary rate %d rejected: %v", rate, err)
		}
	}
}

// The thinking sound must accept a file for the same reason the
// acknowledgement does: the owner made his own and a generated tone is not
// what he chose. A missing file has to fail at load, not become silence
// during the one moment he is waiting and listening for reassurance.
func TestThinkingSoundAcceptsAFileAndValidatesIt(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "thinking.wav")
	pcm := make([]int16, 16000/2)
	for i := range pcm {
		pcm[i] = 4000
	}
	if err := os.WriteFile(good, audio.EncodeWAV(pcm, audio.Format{SampleRate: 16000, Channels: 1}), 0o644); err != nil {
		t.Fatal(err)
	}

	c := Default()
	c.Feedback.Thinking = good
	if err := c.Validate(); err != nil {
		t.Fatalf("a real thinking sound file was rejected: %v", err)
	}

	c.Feedback.Thinking = filepath.Join(dir, "absent.wav")
	if err := c.Validate(); err == nil {
		t.Error("a missing thinking sound file was accepted; it would be silence when he most expects a sound")
	}

	// A mistyped NAME must still be reported as a bad name, listing what is
	// valid, rather than as a missing file.
	c.Feedback.Thinking = "tickk"
	err := c.Validate()
	if err == nil {
		t.Fatal("an unknown thinking name was accepted")
	}
	if strings.Contains(err.Error(), "no such file") {
		t.Errorf("error %q treats a mistyped name as a path", err)
	}
}
