// Package vad splits a continuous PCM stream into utterances.
//
// The detector is energy based (RMS with a hysteresis gate and ambient
// auto-calibration), not a neural model. That is a deliberate trade: it costs
// microseconds per frame, adds no dependency, and its failure mode is a
// harmless extra transcription rather than a missed command. The expensive
// stages (speech-to-text, the model) only ever see segments the gate accepted.
package vad

import (
	"context"

	"github.com/jerryfane/voice/internal/audio"
)

// Utterance is one detected span of speech.
type Utterance struct {
	// PCM is the audio, including the configured pre-roll.
	PCM []int16
	// Format describes the samples.
	Format audio.Format
	// Peak is the loudest frame RMS (0-1), useful for diagnostics.
	Peak float64
	// Truncated reports that MaxUtterance cut the segment short.
	Truncated bool
}

// Params tunes the gate. All durations are converted from config.
type Params struct {
	// Threshold is the RMS gate (0-1). Zero means auto-calibrate.
	Threshold float64
	// MinSpeechFrames, SilenceFrames, MaxFrames and PreRollFrames are
	// expressed in frames of FrameSize samples.
	MinSpeech    int
	Silence      int
	MaxUtterance int
	PreRoll      int
	// FrameSize is the analysis window in samples (typically 20ms worth).
	FrameSize int
}

// Segmenter consumes PCM buffers and emits utterances.
type Segmenter interface {
	// Run reads from in until it closes or ctx is cancelled, emitting one
	// Utterance per detected span. The returned channel closes when input
	// stops, so callers can range over it.
	Run(ctx context.Context, in <-chan []int16, f audio.Format) <-chan Utterance
}
