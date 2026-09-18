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
		ws := skeleton(want)
		cs := skeleton(candidate)
		den := len([]rune(ws))
		if den == 0 {
			continue
		}
		if float64(distance(cs, ws))/float64(den) <= fuzz {
			return true, strings.TrimSpace(strings.Join(words[len(p):], " ")), raw
		}
	}
	return false, "", ""
}

func normalize(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, s)), " "))
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
		case 'h', 'j', 'w':
			class = 'H'
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
