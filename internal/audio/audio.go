// Package audio defines the microphone/speaker contract.
//
// Voice never links an audio library. Capture and playback are delegated to
// whatever command the user configures (arecord/aplay by default, but ffmpeg,
// sox, pw-record or a remote shell work equally well). That keeps the binary
// CGO-free and statically linkable, and means ANY input or output device the
// host OS exposes is usable without a Voice code change.
package audio

import "context"

// Format is the PCM shape Voice works in end to end: signed 16-bit
// little-endian mono. Every engine (VAD, STT, TTS) assumes this.
type Format struct {
	SampleRate int // samples per second, e.g. 16000
	Channels   int // always 1 for the recognition path
}

// Default is the format wake-word detection and speech-to-text expect.
func Default() Format { return Format{SampleRate: 16000, Channels: 1} }

// Recorder streams PCM from an input device until the context is cancelled.
//
// Stream must return promptly; capture runs in the background and pushes
// fixed-size buffers into the returned channel. The samples channel is closed
// when capture stops. Any fatal error is delivered on the error channel before
// the samples channel closes.
type Recorder interface {
	Stream(ctx context.Context) (<-chan []int16, <-chan error)
	// Format reports the PCM shape the recorder emits.
	Format() Format
	// Describe returns a human-readable device identity for `voice doctor`.
	Describe() string
}

// Player renders audio to an output device.
//
// PlayWAV accepts a complete RIFF/WAVE payload (what Piper and most TTS
// engines emit). PlayPCM accepts raw samples in the player's format. Both
// block until playback finishes or the context is cancelled, so callers can
// serialise speech and know when the speaker is idle again.
type Player interface {
	PlayWAV(ctx context.Context, wav []byte) error
	PlayPCM(ctx context.Context, pcm []int16, f Format) error
	// Stop interrupts any in-flight playback (used for barge-in and ducking).
	Stop() error
	Describe() string
}

// Device is an enumerated input or output endpoint, used by `voice devices audio`
// so users can discover the exact name to put in their config.
type Device struct {
	ID       string // value to use in config, e.g. "plughw:2,0" or "default"
	Name     string // human label, e.g. "Anker PowerConf S3"
	Playback bool
	Capture  bool
}
