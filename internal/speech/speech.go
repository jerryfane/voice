// Package speech defines the pluggable speech-to-text and text-to-speech
// contracts.
//
// Engines are external programs described by a command template in the config.
// herdr writes audio to a temp file, runs the template, and reads the result.
// This is deliberate: it lets a user swap whisper.cpp for faster-whisper, a
// cloud endpoint, or a wrapper script without recompiling, and it keeps the
// herdr binary free of CGO and model weights.
package speech

import (
	"context"

	"github.com/jerryfane/herdr-voice/internal/audio"
)

// Transcriber turns captured speech into text.
type Transcriber interface {
	// Transcribe returns the recognised text for one utterance. It returns an
	// empty string (not an error) when the audio contains no intelligible
	// speech, which is a normal outcome for false VAD triggers.
	Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error)
	// Name identifies the engine in logs and `herdr doctor`.
	Name() string
	// Available reports whether the engine's binary and model are present, with
	// a human-readable reason when they are not.
	Available() (bool, string)
}

// Synthesizer turns text into speech audio.
type Synthesizer interface {
	// Synthesize returns a complete RIFF/WAVE payload for the given text.
	Synthesize(ctx context.Context, text string) ([]byte, error)
	Name() string
	Available() (bool, string)
}
