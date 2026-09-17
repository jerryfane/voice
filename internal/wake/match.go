// Package wake recognises configured wake phrases in an STT transcript.
package wake

import (
	"strings"
	"unicode"
)

// Match reports whether transcript starts with a configured phrase within the
// normalised edit-distance tolerance. Requiring the phrase at the beginning
// prevents incidental mentions in household conversation from activating Voice.
func Match(transcript string, phrases []string, fuzz float64) (matched bool, command string, phrase string) {
	words := strings.Fields(normalize(transcript))
	for _, raw := range phrases {
		p := strings.Fields(normalize(raw))
		if len(p) == 0 || len(words) < len(p) {
			continue
		}
		candidate := strings.Join(words[:len(p)], " ")
		want := strings.Join(p, " ")
		d := distance(candidate, want)
		den := len([]rune(want))
		if den == 0 {
			continue
		}
		if float64(d)/float64(den) <= fuzz {
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
