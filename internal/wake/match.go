// Package wake recognises configured wake phrases in an STT transcript.
package wake

import (
	"strings"
	"unicode"
)

// Match reports whether transcript starts with a configured phrase, comparing
// how the words SOUND rather than how they are spelled. Requiring the phrase
// at the beginning prevents incidental mentions in household conversation from
// activating Voice.
//
// Spelling was the wrong measure. On the device, whisper rendered "Hey Voice"
// as "Hey boys": four edits over nine characters, a normalised distance of
// 0.44, so the wake phrase never matched while the microphone and the
// transcription were both working perfectly. No edit-distance tolerance can
// accept that without also accepting ordinary conversation.
//
// The transcript and the phrase are therefore reduced to a consonant skeleton
// that folds the classes speech engines confuse, and the configured tolerance
// applies to that. "voice" and "boys" reduce alike; unrelated speech does not.
func Match(transcript string, phrases []string, fuzz float64) (matched bool, command string, phrase string) {
	words := strings.Fields(normalize(transcript))
	for _, raw := range phrases {
		p := strings.Fields(normalize(raw))
		if len(p) == 0 || len(words) < len(p) {
			continue
		}
		candidate := strings.Join(words[:len(p)], " ")
		want := strings.Join(p, " ")
		if candidate == want {
			return true, strings.TrimSpace(strings.Join(words[len(p):], " ")), raw
		}
		// fuzz == 0 keeps its documented meaning: an exact transcript. Sound
		// matching is a tolerance, so it is governed by the tolerance rather
		// than bypassing it - an earlier version of this change accepted
		// sound-alikes even at zero, which silently broke that contract.
		if fuzz <= 0 {
			continue
		}
		// Per word, not over the joined phrase. A single scalar on the whole
		// skeleton cannot work here and the review proved it: "hey boys" and
		// "the voice" are both distance 1 over a 7-class skeleton, so any
		// threshold accepting the transcript we must accept also accepts a
		// sentence about somebody's voice. Judging each word against its own
		// counterpart keeps the tolerance proportional to that word: a short
		// leading word like "hey" admits no substitution at 0.2, while
		// "voice" can still be heard as "boys".
		if !wordsSoundAlike(words[:len(p)], p, fuzz) {
			continue
		}
		return true, strings.TrimSpace(strings.Join(words[len(p):], " ")), raw
	}
	return false, "", ""
}

// wordTolerant reports whether a word of den sound classes can be matched
// approximately at this tolerance. It is the SINGLE rule used both by Match
// and by what the doctor reports: I wrote those separately once and the review
// found the report drifting from the behaviour it claimed to describe.
//
// Two conditions, for two different reasons. The tolerance must admit at least
// one class change, or "approximate" means nothing and the word is exact in
// practice however it is described - a fixed cutoff cannot know that, because
// it depends on the configured fuzz. And a word of fewer than four classes is
// never tolerated whatever the tolerance says, because short words collide
// outright: "he", "hi" and "hey" are all HF, so a generous fuzz would make
// every two-letter pronoun a wake word.
func wordTolerant(den int, fuzz float64) bool {
	if den < 4 {
		return false
	}
	return 1/float64(den) <= fuzz
}

// ExactOnly reports whether a configured phrase has too little recognisable
// sound for the tolerance to apply, so every word must match exactly: a
// non-Latin phrase whose runes contribute no sound classes, a phrase of short
// words, or one whose words are too short for the configured tolerance to
// admit a single change.
//
// It exists so the degradation is not silent - a user setting wake.fuzz on
// such a phrase would otherwise see the setting quietly do nothing - and it
// answers with the same rule Match applies, so the report cannot drift.
func ExactOnly(phrase string, fuzz float64) bool {
	for _, w := range strings.Fields(normalize(phrase)) {
		if wordTolerant(len([]rune(skeleton(w))), fuzz) {
			return false
		}
	}
	return true
}

// Anchorless reports whether a phrase is a single word, which makes it wake on
// any sound-alike of that word alone: a phrase of "voice" answers the first
// word of "boys will be boys". A multi-word phrase has the rest of itself as
// an anchor; one word has nothing, and calling that safely tolerant - as an
// earlier version of the doctor report did - understates the risk.
func Anchorless(phrase string) bool {
	return len(strings.Fields(normalize(phrase))) < 2
}

func normalize(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, s)), " "))
}

// wordsSoundAlike reports whether every word sounds like its counterpart
// within the tolerance, each judged against its own length.
func wordsSoundAlike(heard, want []string, fuzz float64) bool {
	if len(heard) != len(want) {
		return false
	}
	for i := range want {
		w := skeleton(want[i])
		den := len([]rune(w))
		// A short word carries too little sound to be tolerant about. Two
		// review rounds found zero-distance collisions on the leading word
		// - first "we" and "why" through a fold of mine, then "he" and "hi",
		// which reduce to the same two classes as "hey" no matter what the
		// fold does, because e, i and y are one vowel class and the repeated
		// class collapses. No tolerance can separate those, so a word with
		// fewer than four sound classes must match exactly.
		//
		// This costs nothing real: the confusion that broke the device was in
		// the DISTINCTIVE word - "voice" heard as "boys" - and that word is
		// long enough to keep its tolerance. A recogniser mangling "hey"
		// itself is a different problem, and one that should be solved by
		// configuring the phrase it actually produces rather than by making
		// every two-letter pronoun a wake word.
		if !wordTolerant(den, fuzz) {
			if heard[i] != want[i] {
				return false
			}
			continue
		}
		if den == 0 {
			// Nothing to compare by sound - a non-Latin phrase, for example -
			// so fall back to the spelling this function cannot judge rather
			// than silently accepting anything.
			if heard[i] != want[i] {
				return false
			}
			continue
		}
		if float64(distance(skeleton(heard[i]), w))/float64(den) > fuzz {
			return false
		}
	}
	return true
}

// skeleton reduces text to the sound classes that survive speech recognition.
// Consonant classes are the confusions that actually occur: b/v/f/p share a
// place of articulation, s/z/c/x a sibilant sound, d/t, g/k/q, m/n and l/r
// likewise. Vowels are kept but coarsened to front (e/i/y) and back (a/o/u),
// because dropping them entirely collapses too much: with vowels discarded,
// "hey buzz" matched a configured "hey voice", which would wake Voice during
// ordinary talk. Keeping two vowel classes separates "boys" from "buzz" while
// still treating "boys" and "voice" as near neighbours.
func skeleton(s string) string {
	var b []byte
	for _, r := range s {
		var class byte
		switch r {
		case 'b', 'v', 'f', 'p':
			class = 'B'
		case 's', 'z', 'c', 'x':
			class = 'S'
		case 'd', 't':
			class = 'D'
		case 'g', 'k', 'q':
			class = 'G'
		case 'm', 'n':
			class = 'N'
		case 'l', 'r':
			class = 'L'
		case 'h':
			class = 'H'
		case 'j':
			class = 'J'
		case 'w':
			// w is NOT folded with h. Merging them made "we", "why" and "hey"
			// identical skeletons, so "we voice our support" matched a
			// configured "hey voice" at every tolerance including zero - a
			// collision no threshold could fix. That fold was mine, was never
			// part of the design, and is exactly the kind of undocumented
			// convenience that turns a matcher into a nuisance.
			class = 'W'
		case 'e', 'i', 'y':
			class = 'F' // front vowels
		case 'a', 'o', 'u':
			class = 'A' // back and open vowels
		case ' ':
			continue
		default:
			if r >= '0' && r <= '9' {
				class = byte(r)
			} else {
				continue
			}
		}
		// Collapse a repeated class: "boss" and "bos" sound alike.
		if len(b) > 0 && b[len(b)-1] == class {
			continue
		}
		b = append(b, class)
	}
	return string(b)
}

func distance(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev := make([]int, len(y)+1)
	cur := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 0
			if x[i-1] != y[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(y)]
}
func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
