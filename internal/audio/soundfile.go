package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxSoundFile bounds a custom acknowledgement sound. This is a blip played
// when the speaker starts listening, not a track: a file this size is a
// mistake, and reading it into memory on a Pi is not free.
const maxSoundFile = 8 << 20

// silenceFloor is the amplitude below which a trailing sample counts as
// silence when trimming, unless the sound is quieter than that throughout.
// About -60 dBFS: inaudible next to speech, but well above the dither in a
// generated file.
const silenceFloor = 32

// Sample rates outside this range are not audio anyone meant to play. The
// bound exists because Conform scales the sample count by the rate ratio: a
// 46-byte WAV declaring 1 Hz asks for gigabytes at the session rate, which
// review reproduced as a startup panic - makeslice: len out of range - on the
// owner's device.
const (
	MinSampleRate = 4000
	MaxSampleRate = 192000
)

// maxSoundSeconds bounds the CONFORMED result. The size cap alone cannot: a
// small file can legitimately decode to an enormous one after resampling.
const maxSoundSeconds = 10

// maxSoundSamples is an absolute ceiling, independent of any rate. Deriving
// the limit from the target rate alone was not enough: the session rate comes
// from config and review panicked the startup path with input.sample_rate set
// to MaxInt, where rate * seconds overflows before it can bound anything.
const maxSoundSamples = MaxSampleRate * maxSoundSeconds

// IsSoundFile reports whether a feedback.sound value names a FILE rather than
// one of the built-in generated sounds.
//
// The test is a path separator or an audio extension, not "is it in the list
// of built-ins", so a typo like "chimee" still fails as an unknown NAME with
// the list of valid names, instead of being reported as a missing file.
func IsSoundFile(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	if strings.ContainsRune(n, filepath.Separator) || strings.HasPrefix(n, "~") {
		return true
	}
	switch strings.ToLower(filepath.Ext(n)) {
	case ".wav", ".wave":
		return true
	}
	return false
}

// DecodeWAV reads a 16-bit PCM RIFF/WAVE payload into samples.
//
// Deliberately narrow: 16-bit PCM only, which is what every tool that would
// produce a notification sound can emit, and what the rest of this package
// already speaks. Anything else fails with a message naming the problem,
// because a sound that silently does not play is the failure mode this whole
// feature exists to avoid.
func DecodeWAV(wav []byte) ([]int16, Format, error) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, Format{}, errors.New("not a RIFF/WAVE file")
	}
	var (
		format   Format
		bits     uint16
		data     []byte
		haveFmt  bool
		haveData bool
	)
	for offset := 12; offset+8 <= len(wav); {
		id := string(wav[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		body := offset + 8
		end := body + size
		if size < 0 || end < body || end > len(wav) {
			return nil, Format{}, errors.New("WAVE chunk exceeds the file")
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, Format{}, errors.New("WAVE fmt chunk is too short")
			}
			if enc := binary.LittleEndian.Uint16(wav[body : body+2]); enc != 1 {
				return nil, Format{}, fmt.Errorf("WAVE encoding %d is not PCM; convert it with: ffmpeg -i in -c:a pcm_s16le out.wav", enc)
			}
			format.Channels = int(binary.LittleEndian.Uint16(wav[body+2 : body+4]))
			format.SampleRate = int(binary.LittleEndian.Uint32(wav[body+4 : body+8]))
			bits = binary.LittleEndian.Uint16(wav[body+14 : body+16])
			haveFmt = true
		case "data":
			data = wav[body:end]
			haveData = true
		}
		offset = end
		if size%2 == 1 {
			offset++ // chunks are word aligned
		}
	}
	switch {
	case !haveFmt:
		return nil, Format{}, errors.New("WAVE file has no fmt chunk")
	case !haveData:
		return nil, Format{}, errors.New("WAVE file has no data chunk")
	case bits != 16:
		return nil, Format{}, fmt.Errorf("WAVE is %d-bit; only 16-bit PCM is supported, convert with: ffmpeg -i in -c:a pcm_s16le out.wav", bits)
	case format.Channels < 1:
		return nil, Format{}, errors.New("WAVE reports no channels")
	case format.SampleRate < MinSampleRate || format.SampleRate > MaxSampleRate:
		return nil, Format{}, fmt.Errorf("WAVE sample rate %d is outside %d-%d Hz", format.SampleRate, MinSampleRate, MaxSampleRate)
	case len(data)%2 != 0:
		// F3: a dangling byte means the file is damaged or mis-declared.
		// Dropping it silently would play a truncated sound and hide that.
		return nil, Format{}, fmt.Errorf("WAVE data chunk is %d bytes, not a whole number of 16-bit samples", len(data))
	case (len(data)/2)%format.Channels != 0:
		return nil, Format{}, fmt.Errorf("WAVE data holds %d samples, not a whole number of %d-channel frames", len(data)/2, format.Channels)
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i : 2*i+2]))
	}
	return pcm, format, nil
}

// Conform converts samples to the target format: extra channels are mixed
// down, and the rate is resampled linearly.
//
// Linear interpolation is chosen knowingly. It is not what you would use on
// music, but this is a sub-second notification blip; the artefacts sit far
// above what a speakerphone reproduces, and the alternative is either a
// filter nobody here will tune or a third-party dependency this repo does
// not allow.
func Conform(pcm []int16, from, to Format) []int16 {
	if len(pcm) == 0 {
		return nil
	}
	if from.Channels > 1 {
		frames := len(pcm) / from.Channels
		mono := make([]int16, frames)
		for i := range mono {
			sum := 0
			for c := 0; c < from.Channels; c++ {
				sum += int(pcm[i*from.Channels+c])
			}
			mono[i] = int16(sum / from.Channels)
		}
		pcm = mono
	}
	if to.SampleRate <= 0 || from.SampleRate <= 0 || from.SampleRate == to.SampleRate {
		return pcm
	}
	// Computed in float64 and clamped against an absolute ceiling before any
	// conversion to int, so neither the ratio nor the limit can overflow.
	ratio := float64(to.SampleRate) / float64(from.SampleRate)
	want := float64(len(pcm)) * ratio
	if want > float64(maxSoundSamples) {
		want = float64(maxSoundSamples)
	}
	if want < 0 {
		return nil
	}
	out := make([]int16, int(want))
	for i := range out {
		pos := float64(i) / ratio
		left := int(pos)
		if left >= len(pcm)-1 {
			out[i] = pcm[len(pcm)-1]
			continue
		}
		frac := pos - float64(left)
		out[i] = int16(float64(pcm[left])*(1-frac) + float64(pcm[left+1])*frac)
	}
	return out
}

// TrimTrailingSilence drops silence from the end of a sound.
//
// A generator commonly pads to a round duration: the owner's own clip is
// 0.504 s long and stops making sound at 0.186 s. That padding is not
// harmless here - playback holds the speaker open for its whole length while
// the microphone is already listening for the command, so the trailing
// silence is dead time between the user hearing the sound and being heard.
func TrimTrailingSilence(pcm []int16) []int16 {
	// The threshold follows the sound rather than being absolute. A fixed
	// floor destroys a deliberately quiet blip entirely - review showed a
	// valid file peaking at amplitude 32 trimmed to nothing and then
	// rejected as inaudible. Relative to its own peak, a soft sound keeps
	// its shape and only genuine padding goes.
	peak := 0
	for _, v := range pcm {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	if peak == 0 {
		return nil
	}
	floor := peak / 64
	if floor > silenceFloor {
		floor = silenceFloor
	}
	end := len(pcm)
	for end > 0 {
		v := int(pcm[end-1])
		if v < 0 {
			v = -v
		}
		if v > floor {
			break
		}
		end--
	}
	return pcm[:end]
}

// SoundOptions describes how a loaded sound will be used, because the two
// settings that accept a file want opposite treatment of trailing silence.
type SoundOptions struct {
	// Setting names the config field, so a failure points at the thing the
	// operator has to change rather than at whichever loader was reused.
	Setting string
	// KeepTrailingSilence preserves padding at the end of the file.
	//
	// For the acknowledgement, padding is dead time while the microphone is
	// already listening, so it goes. For the THINKING sound the file is
	// looped, the silence after the pulse IS the gap between pulses, and
	// trimming it turns a once-per-second tick into a continuous rattle -
	// while the documentation tells the user to put that silence there.
	KeepTrailingSilence bool
	// MinDuration rejects a sound too short to be used the way this setting
	// plays it. A 20 ms looped file restarts the player forty times a
	// second.
	MinDuration time.Duration
}

// SoundFromFile loads a custom sound, conformed to f and scaled by volume.
func SoundFromFile(path string, f Format, volume float64, opt SoundOptions) ([]int16, error) {
	setting := opt.Setting
	if setting == "" {
		setting = "sound"
	}
	// Opened ONCE and inspected through that descriptor. Stat-then-ReadFile
	// checked one file and read another: review showed a FIFO reporting size
	// zero and hanging startup until the process was killed, and a path
	// swapped between the two calls bypassing the size check entirely.
	// O_NONBLOCK because opening a FIFO for reading BLOCKS until a writer
	// appears - the open itself hangs, before any Stat can reject it. With
	// the flag, a pipe opens immediately and is then refused below.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", setting, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", setting, statErr), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(
			fmt.Errorf("%s %s is not a regular file (%s); a pipe or device can never finish loading", setting, path, info.Mode().Type()),
			file.Close())
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxSoundFile+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(fmt.Errorf("%s %s", setting, path), readErr, closeErr)
	}
	if len(raw) > maxSoundFile {
		return nil, fmt.Errorf("%s %s exceeds %d bytes", setting, path, maxSoundFile)
	}
	pcm, from, err := DecodeWAV(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", setting, path, err)
	}
	// Checked BEFORE conforming, and reported rather than silently clamped:
	// a sound quietly cut to ten seconds is still wrong, and the caller
	// deserves to know the file is not what an acknowledgement sound should
	// be. This also covers the same-rate path, where no resampling happens
	// and the byte cap alone would allow minutes of audio.
	frames := int64(len(pcm)) / int64(max(from.Channels, 1))
	if f.SampleRate > 0 && from.SampleRate > 0 {
		out := frames * int64(f.SampleRate) / int64(from.SampleRate)
		if out > int64(maxSoundSamples) || out > int64(f.SampleRate)*int64(maxSoundSeconds) {
			return nil, fmt.Errorf("%s %s is longer than %d seconds once converted", setting, path, maxSoundSeconds)
		}
	}
	pcm = Conform(pcm, from, f)
	if !opt.KeepTrailingSilence {
		pcm = TrimTrailingSilence(pcm)
	} else if len(TrimTrailingSilence(pcm)) == 0 {
		// Keeping the padding is right, but a file that is ONLY padding is
		// not a sound.
		return nil, fmt.Errorf("%s %s contains no audible audio", setting, path)
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("%s %s contains no audible audio", setting, path)
	}
	if opt.MinDuration > 0 && f.SampleRate > 0 {
		if got := time.Duration(len(pcm)) * time.Second / time.Duration(f.SampleRate); got < opt.MinDuration {
			return nil, fmt.Errorf("%s %s is %v once converted; it must be at least %v, or looping it restarts the player continuously",
				setting, path, got.Round(time.Millisecond), opt.MinDuration)
		}
	}
	volume = math.Max(0, math.Min(1, volume))
	if volume == 0 {
		return nil, nil
	}
	if volume < 1 {
		for i := range pcm {
			pcm[i] = int16(float64(pcm[i]) * volume)
		}
	}
	return pcm, nil
}

// The two settings that accept a file, and how each one needs it treated.
var (
	// AcknowledgementSound plays once when a wake phrase is accepted.
	// Trailing silence is dead time while the microphone is already
	// listening, so it is removed.
	AcknowledgementSound = SoundOptions{Setting: "feedback.sound"}

	// ThinkingSound is LOOPED while an answer is slow. The silence after the
	// pulse is the gap between pulses, so it is kept, and a file too short
	// to loop calmly is refused rather than turned into a rattle.
	ThinkingSound = SoundOptions{
		Setting:             "feedback.thinking",
		KeepTrailingSilence: true,
		MinDuration:         250 * time.Millisecond,
	}
)
