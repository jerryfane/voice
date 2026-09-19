package audio

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
	"time"
)

func TestPrependWAVSilencePreservesHeaderAndSpeech(t *testing.T) {
	original := EncodeWAV([]int16{1, -2, 300, -400}, Format{SampleRate: 16000, Channels: 1})
	got, err := PrependWAVSilence(original, 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	const silenceBytes = 16000 * 2 / 4
	if want := len(original) + silenceBytes; len(got) != want {
		t.Fatalf("padded length = %d, want %d", len(got), want)
	}
	if gotSize, want := binary.LittleEndian.Uint32(got[4:8]), uint32(len(got)-8); gotSize != want {
		t.Fatalf("RIFF size = %d, want %d", gotSize, want)
	}
	if gotSize, want := binary.LittleEndian.Uint32(got[40:44]), uint32(len(original)-44+silenceBytes); gotSize != want {
		t.Fatalf("data size = %d, want %d", gotSize, want)
	}
	if silence := got[44 : 44+silenceBytes]; !bytes.Equal(silence, make([]byte, silenceBytes)) {
		t.Fatal("speech lead-in contains non-silent samples")
	}
	if speech := got[44+silenceBytes:]; !bytes.Equal(speech, original[44:]) {
		t.Fatal("padding changed the synthesized speech payload")
	}
}

func TestPrependPCMSilencePreservesEarcon(t *testing.T) {
	original := []int16{1, -2, 300, -400}
	got := PrependPCMSilence(original, Format{SampleRate: 16000, Channels: 1}, 250*time.Millisecond)

	const silenceSamples = 16000 / 4
	if want := len(original) + silenceSamples; len(got) != want {
		t.Fatalf("padded length = %d, want %d", len(got), want)
	}
	for i, sample := range got[:silenceSamples] {
		if sample != 0 {
			t.Fatalf("lead-in sample %d = %d, want silence", i, sample)
		}
	}
	if !slices.Equal(got[silenceSamples:], original) {
		t.Fatalf("earcon = %v, want %v", got[silenceSamples:], original)
	}
}
