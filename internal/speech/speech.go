// Package speech defines the pluggable speech-to-text and text-to-speech
// contracts.
//
// Engines are external programs described by a command template in the config.
// Voice writes audio to a temp file, runs the template, and reads the result.
// This is deliberate: it lets a user swap whisper.cpp for faster-whisper, a
// cloud endpoint, or a wrapper script without recompiling, and it keeps the
// Voice binary free of CGO and model weights.
package speech

import (
	"context"
	"strings"

	"github.com/jerryfane/voice/internal/audio"
)

// Transcriber turns captured speech into text.
type Transcriber interface {
	// Transcribe returns the recognised text for one utterance. It returns an
	// empty string (not an error) when the audio contains no intelligible
	// speech, which is a normal outcome for false VAD triggers.
	Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error)
	// Name identifies the engine in logs and `voice doctor`.
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

// SplitDelivery extracts an optional leading [voice: direction] directive.
// Directions must be nonempty, single-line, and contain no nested brackets.
// Valid metadata is removed and the remaining speech is trimmed; other text,
// including ordinary brackets and malformed or inline directives, is unchanged.
// A directive without speech returns an empty spoken string.
func SplitDelivery(text string) (direction, spoken string) {
	leading := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(leading, "[voice:") {
		return "", text
	}
	end := strings.IndexByte(leading, ']')
	if end < 0 {
		return "", text
	}
	raw := leading[len("[voice:"):end]
	if strings.ContainsAny(raw, "[\r\n\u0085\u2028\u2029") {
		return "", text
	}
	direction = strings.TrimSpace(raw)
	if direction == "" {
		return "", text
	}
	return direction, strings.TrimSpace(leading[end+1:])
}
