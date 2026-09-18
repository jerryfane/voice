package wake

import (
	"testing"

	"github.com/jerryfane/voice/internal/config"
)

func TestMatchExtractsInlineCommand(t *testing.T) {
	ok, cmd, phrase := Match("Hey, VOICE! turn the bedroom light purple", []string{"hey voice"}, 0.2)
	if !ok || cmd != "turn the bedroom light purple" || phrase != "hey voice" {
		t.Fatalf("got ok=%v cmd=%q phrase=%q", ok, cmd, phrase)
	}
}
func TestMatchToleratesSTTSpelling(t *testing.T) {
	ok, _, _ := Match("hey voise what time is it", []string{"hey voice"}, 0.25)
	if !ok {
		t.Fatal("expected fuzzy match")
	}
}
func TestNoMatchInsideUnrelatedSpeech(t *testing.T) {
	ok, _, _ := Match("the speaker was quiet", []string{"hey voice"}, 0.2)
	if ok {
		t.Fatal("unexpected wake match")
	}
}

func TestNoMatchWhenWakePhraseIsEmbeddedInConversation(t *testing.T) {
	ok, _, _ := Match("I heard someone say hey voice turn on the light", []string{"hey voice"}, 0)
	if ok {
		t.Fatal("wake phrase must start the transcript")
	}
}

func TestExactModeRejectsApproximateWakePhrase(t *testing.T) {
	ok, _, _ := Match("hey voise turn on the light", []string{"hey voice"}, 0)
	if ok {
		t.Fatal("exact mode must reject approximate wake phrases")
	}
}

// The device case. Whisper rendered the owner's "Hey Voice" as "Hey boys" and
// nothing happened: four edits over nine characters is a normalised distance
// of 0.44, so no sane spelling tolerance could have matched it. Matching on
// sound does, and this is the exact transcript from the journal.
func TestMatchAcceptsWhatTheRecogniserActuallyHeard(t *testing.T) {
	phrases := []string{"hey voice"}
	for _, heard := range []string{
		"Hey boys, what time is it", // verbatim from the device journal
		"Hey Voice, what time is it",
		"hey voise what time is it",
	} {
		matched, command, _ := Match(heard, phrases, 0.2)
		if !matched {
			t.Errorf("%q did not match %q; the recogniser's spelling must not decide this", heard, phrases[0])
			continue
		}
		if command != "what time is it" {
			t.Errorf("%q gave command %q, want %q", heard, command, "what time is it")
		}
	}
}

// The other direction, which matters more: a matcher that accepts everything
// is worse than one that accepts nothing, because it wakes during ordinary
// talk. "hey buzz light year" is the case that caught me - with vowels
// discarded entirely it collided with "hey voice", so the skeleton keeps two
// coarse vowel classes.
func TestMatchRejectsOrdinarySpeech(t *testing.T) {
	phrases := []string{"hey voice"}
	for _, heard := range []string{
		"hey there",
		"hey mum",
		"hey what time is it",
		"hey buzz light year",
		"the boys are playing outside",
		"they always do",
		"how's school everything's fine",
		"I see the predator sees",
		"a voice in the crowd", // the phrase must start the transcript
	} {
		if matched, _, phrase := Match(heard, phrases, 0.2); matched {
			t.Errorf("%q matched %q; this would wake Voice during ordinary conversation", heard, phrase)
		}
	}
}

// Sound classes must fold the confusions that occur and no more. Checked
// directly because the matcher's discrimination is only as good as this.
func TestSkeletonFoldsConfusableSoundsAndSeparatesOthers(t *testing.T) {
	same := [][2]string{
		{"voice", "voise"}, // s and c are one sibilant class
		{"boss", "bos"},    // a repeated class collapses
		{"bat", "pad"},     // b/p and t/d are one class each
		{"vine", "fine"},   // v and f are one class
	}
	for _, pair := range same {
		if a, b := skeleton(pair[0]), skeleton(pair[1]); a != b {
			t.Errorf("skeleton(%q)=%q and skeleton(%q)=%q should agree", pair[0], a, pair[1], b)
		}
	}
	differ := [][2]string{
		{"voice", "buzz"}, // the false accept that vowel classes prevent
		{"voice", "mum"},
		{"hey", "there"},
		// c heard as an s and k as a g really do differ here. I first
		// asserted these agreed, which was my assumption about the folding
		// rather than a fact about it.
		{"sink", "zinc"},
	}
	for _, pair := range differ {
		if a, b := skeleton(pair[0]), skeleton(pair[1]); a == b {
			t.Errorf("skeleton(%q) and skeleton(%q) both %q; they do not sound alike", pair[0], pair[1], a)
		}
	}
}

// The shipped default has to work on real hardware, which the previous one did
// not: fuzz was 0, so the exact-match requirement rejected what the recogniser
// actually produced and Voice never woke. Asserting the number alone would
// pass any non-zero value, so this asserts the default WAKES on the transcript
// recorded from the device.
func TestShippedDefaultWakesOnTheRecordedDeviceTranscript(t *testing.T) {
	c := config.Default()
	if len(c.Wake.Phrases) == 0 {
		t.Fatal("default config ships no wake phrase")
	}
	matched, command, _ := Match("Hey boys, what time is it", c.Wake.Phrases, c.Wake.Fuzz)
	if !matched {
		t.Fatalf("default config (phrases %q, fuzz %v) does not wake on the transcript the device produced; that default is unusable", c.Wake.Phrases, c.Wake.Fuzz)
	}
	if command != "what time is it" {
		t.Errorf("command = %q, want %q", command, "what time is it")
	}

	// And it must still refuse ordinary speech with those same defaults.
	if matched, _, _ := Match("hey buzz light year", c.Wake.Phrases, c.Wake.Fuzz); matched {
		t.Error("default config wakes on ordinary speech")
	}
}
