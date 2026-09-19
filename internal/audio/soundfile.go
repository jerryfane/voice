package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// maxSoundFile bounds a custom acknowledgement sound. This is a blip played
// when the speaker starts listening, not a track: a file this size is a
// mistake, and reading it into memory on a Pi is not free.
const maxSoundFile = 8 << 20

// silenceFloor is the amplitude below which a trailing sample counts as
// silence when trimming. About -60 dBFS: inaudible next to speech, but well
// above the dither in a generated file.
const silenceFloor = 32

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
	case format.SampleRate <= 0:
		return nil, Format{}, errors.New("WAVE reports no sample rate")
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
	ratio := float64(to.SampleRate) / float64(from.SampleRate)
	out := make([]int16, int(float64(len(pcm))*ratio))
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
	end := len(pcm)
	for end > 0 {
		v := pcm[end-1]
		if v > silenceFloor || v < -silenceFloor {
			break
		}
		end--
	}
	return pcm[:end]
}

// SoundFromFile loads a custom acknowledgement sound, conformed to f and
// scaled by volume.
func SoundFromFile(path string, f Format, volume float64) ([]int16, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("wake sound: %w", err)
	}
	if info.Size() > maxSoundFile {
		return nil, fmt.Errorf("wake sound %s is %d bytes; an acknowledgement sound is a blip, not a track", path, info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wake sound: %w", err)
	}
	pcm, from, err := DecodeWAV(raw)
	if err != nil {
		return nil, fmt.Errorf("wake sound %s: %w", path, err)
	}
	pcm = TrimTrailingSilence(Conform(pcm, from, f))
	if len(pcm) == 0 {
		return nil, fmt.Errorf("wake sound %s contains no audible audio", path)
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
