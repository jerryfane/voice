package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeWAV(t *testing.T, dir string, pcm []int16, f Format) string {
	t.Helper()
	p := filepath.Join(dir, "sound.wav")
	if err := os.WriteFile(p, EncodeWAV(pcm, f), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A file has to survive the round trip, or a sound the owner chose plays as
// something else - or as nothing.
func TestDecodeWAVRoundTripsWhatEncodeWAVWrote(t *testing.T) {
	want := []int16{0, 1000, -1000, 32767, -32768, 7}
	f := Format{SampleRate: 24000, Channels: 1}
	got, gotFormat, err := DecodeWAV(EncodeWAV(want, f))
	if err != nil {
		t.Fatal(err)
	}
	if gotFormat != f {
		t.Errorf("format = %+v, want %+v", gotFormat, f)
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

// The failure that matters is a file that cannot play. Each of these must say
// what is wrong rather than yield silence, because silence is exactly what
// the user would otherwise have to diagnose by ear.
func TestDecodeWAVRejectsWhatItCannotPlay(t *testing.T) {
	good := EncodeWAV([]int16{1, 2, 3}, Format{SampleRate: 16000, Channels: 1})
	cases := map[string][]byte{
		"not a wav at all":  []byte("ID3\x04 this is an mp3"),
		"truncated header":  good[:8],
		"chunk beyond file": append(append([]byte{}, good[:4]...), []byte("XXXXWAVEfmt \xff\xff\xff\xff")...),
	}
	for name, body := range cases {
		if _, _, err := DecodeWAV(body); err == nil {
			t.Errorf("%s was accepted; a sound that cannot decode must fail loudly", name)
		}
	}
}

// 24-bit and float WAVs are common exports. They must be refused with advice,
// not accepted and played as noise.
func TestDecodeWAVRefusesNon16BitWithConversionAdvice(t *testing.T) {
	wav := append([]byte{}, EncodeWAV([]int16{1, 2, 3}, Format{SampleRate: 16000, Channels: 1})...)
	wav[34] = 24 // bits per sample in the fmt chunk
	_, _, err := DecodeWAV(wav)
	if err == nil {
		t.Fatal("a 24-bit WAV was accepted")
	}
	if got := err.Error(); !contains(got, "16-bit") || !contains(got, "ffmpeg") {
		t.Errorf("error %q should name the limitation and how to convert", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// Stereo at a different rate is what a person actually downloads from a sound
// generator. It must end up mono at the session rate, or playback is garbled
// or wrongly pitched.
func TestConformDownmixesAndResamples(t *testing.T) {
	from := Format{SampleRate: 48000, Channels: 2}
	to := Format{SampleRate: 16000, Channels: 1}
	// 6 stereo frames: left and right differ so a downmix bug is visible.
	stereo := []int16{100, 300, 100, 300, 100, 300, 100, 300, 100, 300, 100, 300}

	got := Conform(stereo, from, to)
	if want := 2; len(got) != want {
		t.Fatalf("%d samples at 16 kHz from 6 frames at 48 kHz, want %d", len(got), want)
	}
	for i, s := range got {
		if s < 180 || s > 220 {
			t.Errorf("sample %d = %d, want the ~200 average of 100 and 300: a channel was dropped rather than mixed", i, s)
		}
	}
}

// Padding is not harmless: playback holds the speaker open for the file's
// whole length while the microphone is already listening.
func TestTrimTrailingSilenceKeepsTheSoundAndDropsThePadding(t *testing.T) {
	pcm := append([]int16{5000, -5000, 3000}, make([]int16, 4000)...)
	got := TrimTrailingSilence(pcm)
	if len(got) != 3 {
		t.Errorf("trimmed to %d samples, want 3: the padding was kept or the sound was eaten", len(got))
	}
	if all := TrimTrailingSilence(make([]int16, 100)); len(all) != 0 {
		t.Errorf("a fully silent file trimmed to %d samples, want 0", len(all))
	}
}

// End to end on a real path, which is what the config actually configures.
func TestSoundFromFileConformsScalesAndTrims(t *testing.T) {
	dir := t.TempDir()
	loud := make([]int16, 480)
	for i := range loud {
		loud[i] = 20000
	}
	path := writeWAV(t, dir, append(loud, make([]int16, 4800)...), Format{SampleRate: 48000, Channels: 1})

	got, err := SoundFromFile(path, Format{SampleRate: 16000, Channels: 1}, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no audio returned")
	}
	if len(got) > 200 {
		t.Errorf("%d samples: trailing silence was not trimmed after resampling", len(got))
	}
	peak := 0
	for _, s := range got {
		if int(s) > peak {
			peak = int(s)
		}
	}
	if peak > 11000 || peak < 9000 {
		t.Errorf("peak %d, want about half of 20000: volume was not applied", peak)
	}
}

// A missing file must fail at load, when someone can read the message, rather
// than at the moment the owner speaks to the device.
func TestSoundFromFileReportsAMissingFile(t *testing.T) {
	_, err := SoundFromFile(filepath.Join(t.TempDir(), "nope.wav"), Default(), 0.65)
	if err == nil {
		t.Fatal("a missing sound file was accepted")
	}
}

// Telling a path from a built-in name decides which error the user gets. A
// typo in a name must still list the valid names.
func TestIsSoundFileDistinguishesPathsFromNames(t *testing.T) {
	for _, name := range []string{"chime", "blip", "two-up", "none", "", "chimee"} {
		if IsSoundFile(name) {
			t.Errorf("%q treated as a file; a mistyped name must report the valid names instead", name)
		}
	}
	for _, path := range []string{"/etc/voice/bubble.wav", "./sound.wav", "sounds/x.WAV", "~/bubble.wave"} {
		if !IsSoundFile(path) {
			t.Errorf("%q not treated as a file", path)
		}
	}
}

// A tiny WAV declaring an absurd sample rate asks Conform to scale the sample
// count by the rate ratio. Review reproduced a 46-byte file claiming 1 Hz
// panicking with "makeslice: len out of range" while the service was
// STARTING - a file the owner could be handed is a way to stop his speaker
// booting, so this must be an error, not a crash.
func TestDecodeWAVRefusesAbsurdSampleRates(t *testing.T) {
	for _, rate := range []int{1, 100, 3999, 192001, 1 << 30} {
		wav := EncodeWAV([]int16{1, 2, 3, 4}, Format{SampleRate: rate, Channels: 1})
		if _, _, err := DecodeWAV(wav); err == nil {
			t.Errorf("sample rate %d was accepted; resampling it would allocate without bound", rate)
		}
	}
	for _, rate := range []int{8000, 16000, 22050, 44100, 48000, 192000} {
		wav := EncodeWAV([]int16{1, 2, 3, 4}, Format{SampleRate: rate, Channels: 1})
		if _, _, err := DecodeWAV(wav); err != nil {
			t.Errorf("ordinary rate %d was rejected: %v", rate, err)
		}
	}
}

// Even within accepted rates the conformed result must be bounded: upsampling
// a legitimately large file must not try to allocate the machine.
func TestConformBoundsTheResult(t *testing.T) {
	from := Format{SampleRate: 4000, Channels: 1}
	to := Format{SampleRate: 192000, Channels: 1}
	got := Conform(make([]int16, 4000*60), from, to) // a minute, upsampled 48x
	if max := to.SampleRate * maxSoundSeconds; len(got) > max {
		t.Errorf("conformed to %d samples, want at most %d", len(got), max)
	}
}

// A FIFO reports size zero and never ends. Stat-then-ReadFile checked one
// thing and read another; review hung startup this way until the process was
// killed.
func TestSoundFromFileRefusesAPipe(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "sound.wav")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := SoundFromFile(fifo, Format{SampleRate: 16000, Channels: 1}, 0.65)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was accepted as a sound file")
		}
		// The REASON matters: with O_NONBLOCK a pipe also fails later as
		// "not a RIFF file", so a test that accepts any error passes even
		// when the regular-file check is removed. Assert the check that is
		// actually protecting startup.
		if !contains(err.Error(), "not a regular file") {
			t.Errorf("error %q does not name the pipe; the regular-file guard may be gone", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SoundFromFile blocked on a FIFO; startup would hang")
	}
}

// Damaged audio must be reported, not quietly shortened: a truncated sound
// that still plays hides the fact that the file is wrong.
func TestDecodeWAVRejectsIncompleteSamplesAndFrames(t *testing.T) {
	base := EncodeWAV([]int16{1, 2, 3, 4}, Format{SampleRate: 16000, Channels: 1})

	// A data chunk of three bytes: not a whole number of 16-bit samples.
	odd := wavWithData(t, Format{SampleRate: 16000, Channels: 1}, []byte{1, 2, 3})
	if _, _, err := DecodeWAV(odd); err == nil {
		t.Error("a data chunk with a dangling byte was accepted")
	}
	_ = base

	stereo := EncodeWAV([]int16{1, 2, 3}, Format{SampleRate: 16000, Channels: 2})
	if _, _, err := DecodeWAV(stereo); err == nil {
		t.Error("three samples in a two-channel file was accepted; that is not a whole number of frames")
	}
}

// wavWithData builds a RIFF file with an arbitrary data payload, so a
// malformed body can be tested without hand-patching offsets in a valid file.
func wavWithData(t *testing.T, f Format, data []byte) []byte {
	t.Helper()
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(36+len(data)))
	out = append(out, "WAVEfmt "...)
	out = binary.LittleEndian.AppendUint32(out, 16)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint16(out, uint16(f.Channels))
	out = binary.LittleEndian.AppendUint32(out, uint32(f.SampleRate))
	out = binary.LittleEndian.AppendUint32(out, uint32(f.SampleRate*f.Channels*2))
	out = binary.LittleEndian.AppendUint16(out, uint16(f.Channels*2))
	out = binary.LittleEndian.AppendUint16(out, 16)
	out = append(out, "data"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

// A deliberately quiet sound must survive. An absolute floor destroyed a
// valid file peaking at amplitude 32 and then rejected it as inaudible.
func TestTrimKeepsADeliberatelyQuietSound(t *testing.T) {
	quiet := []int16{30, -32, 28, -30, 25}
	got := TrimTrailingSilence(quiet)
	if len(got) != len(quiet) {
		t.Errorf("a quiet sound trimmed from %d to %d samples; softness is not silence", len(quiet), len(got))
	}

	// ...while padding after a quiet sound still goes.
	padded := append(append([]int16{}, quiet...), make([]int16, 500)...)
	if got := TrimTrailingSilence(padded); len(got) != len(quiet) {
		t.Errorf("padding after a quiet sound trimmed to %d samples, want %d", len(got), len(quiet))
	}
}
