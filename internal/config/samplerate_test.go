package config

import "testing"

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
