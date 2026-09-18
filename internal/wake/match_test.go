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

		// The review's corpus, and it demolished my first design. Ten
		// ordinary sentences about somebody's voice all matched a configured
		// "hey voice" at the shipped tolerance, because I compared a single
		// scalar over the whole joined skeleton: "the voice" and "hey boys"
		// are BOTH distance 1 over a 7-class skeleton, so no threshold could
		// accept the transcript we need and reject these.
		"her voice was shaking when she called",
		"his voice cracked on the last word",
		"the voice on the radio said otherwise",
		"my voice sounds funny on recordings",
		"we voice our concerns at the meeting",
		"why voice memos keep failing is a mystery",
		"their voice carried across the room",
		"she voiced her objection clearly",
		"a voice note arrived this morning",
		"no voice mail today",
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
		// The zero-distance collision the review found. Folding w with h made
		// these identical, so "we voice our support" matched at EVERY
		// tolerance including zero - unfixable by any threshold.
		{"hey", "we"},
		{"hey", "why"},
		{"hey", "the"},
		{"hey", "her"},
		{"hey", "my"},
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

// Tolerance is per word, because a scalar over the joined phrase provably
// cannot separate the case we must accept from one we must reject: the review
// measured distance(skeleton("hey boys"), skeleton("hey voice")) == 1 over a
// 7-class denominator, and distance(skeleton("the voice"), skeleton("hey
// voice")) == 1 over the same denominator. Identical ratios, opposite
// required answers.
func TestToleranceIsJudgedPerWordNotAcrossThePhrase(t *testing.T) {
	phrases := []string{"hey voice"}

	// The leading word admits no substitution at this tolerance, because its
	// skeleton is short: one class wrong in two is 0.5.
	for _, heard := range []string{"the voice", "we voice", "her voice", "my voice", "why voice"} {
		if matched, _, _ := Match(heard+" tell me something", phrases, 0.2); matched {
			t.Errorf("%q matched; the wake word must not be substitutable", heard)
		}
	}

	// The distinctive word still tolerates what the recogniser does to it.
	for _, heard := range []string{"hey boys", "hey voise", "hey voic"} {
		if matched, _, _ := Match(heard+" tell me something", phrases, 0.2); !matched {
			t.Errorf("%q did not match; the recogniser's spelling of the wake word must not decide this", heard)
		}
	}

	// A transcript with fewer or more words in the phrase position is not a
	// near miss, it is a different utterance.
	if matched, _, _ := Match("hey", phrases, 0.2); matched {
		t.Error("a one-word transcript matched a two-word phrase")
	}
}

// The second zero-distance collision the review found, and it is structural
// rather than a fold of mine: "he" and "hi" reduce to the same two classes as
// "hey", because e, i and y are one vowel class and the repeated class
// collapses. No tolerance can separate them, so a word with fewer than four
// sound classes must match exactly.
func TestShortWakeWordsAreNotSubstitutable(t *testing.T) {
	phrases := []string{"hey voice"}
	for _, heard := range []string{
		"he voiced his concerns about the plan",
		"hi voice mail is full",
		"hay voice",
		"ay voice",
		"a voice",
	} {
		if matched, _, _ := Match(heard, phrases, 0.2); matched {
			t.Errorf("%q matched; a two-letter word must not stand in for the wake word", heard)
		}
	}

	// The distinctive word keeps its tolerance, which is the whole point:
	// that is where the recogniser actually failed on the device.
	if matched, command, _ := Match("hey boys what time is it", phrases, 0.2); !matched || command != "what time is it" {
		t.Errorf("the device transcript stopped matching: matched=%v command=%q", matched, command)
	}
}

// A phrase the tolerance cannot help must say so, rather than leaving a user
// who set wake.fuzz wondering why it does nothing.
func TestExactOnlyReportsPhrasesTheToleranceCannotHelp(t *testing.T) {
	for _, phrase := range []string{"hey", "hi bo", "こんにちは"} {
		if !ExactOnly(phrase) {
			t.Errorf("ExactOnly(%q) = false; this phrase has no word long enough to tolerate", phrase)
		}
	}
	for _, phrase := range []string{"hey voice", "voice", "okay computer"} {
		if ExactOnly(phrase) {
			t.Errorf("ExactOnly(%q) = true; this phrase has sound the tolerance can work with", phrase)
		}
	}
}
