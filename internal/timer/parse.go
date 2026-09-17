package timer

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind is what a spoken timer command asks for.
type Kind int

const (
	// None means the transcript is not a timer command and belongs to the
	// planner.
	None Kind = iota
	Set
	Cancel
	List
)

// Command is a parsed spoken timer request.
type Command struct {
	Kind Kind
	// Duration is set for Set, and for Cancel when the user named one
	// ("cancel the ten minute timer").
	Duration time.Duration
	// Label is the spoken duration ("ten minute"), used when addressing and
	// announcing the timer.
	Label string
	// All is true for "cancel all timers".
	All bool
}

// Parse recognises the local timer vocabulary. Two failure modes matter and
// they pull in opposite directions: intercepting a sentence that belonged to
// the planner ("start the timer app") wastes the user's request, and setting a
// duration the user did not ask for ("the movie is 20 minutes long, set a
// timer for 10 minutes") is worse still. Both are avoided by anchoring
// everything to the word "timer": a duration counts only if it sits against
// that word, and a bare set request counts only if nothing else is in the
// sentence.
func Parse(text string) Command {
	words := strings.Fields(normalise(text))
	anchor := -1
	for i, w := range words {
		if w == "timer" || w == "timers" {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return Command{}
	}
	n := " " + strings.Join(words, " ") + " "

	if verbAt(words, 0, cancelVerbs) > 0 {
		c := Command{Kind: Cancel}
		if contains(words, "all") || contains(words, "every") {
			c.All = true
			return c
		}
		if d, label, ok := anchoredDuration(words, anchor); ok {
			c.Duration, c.Label = d, label
		}
		return c
	}
	if isQuery(n, words) {
		return Command{Kind: List}
	}
	if d, label, ok := anchoredDuration(words, anchor); ok {
		return Command{Kind: Set, Duration: d, Label: label}
	}
	// A set request with no duration: ask how long, but only when the sentence
	// is nothing but that request. "Set the timer for the oven aside" is not.
	if bareSetRequest(words, anchor) {
		return Command{Kind: Set}
	}
	return Command{}
}

var (
	cancelVerbs = [][]string{{"cancel"}, {"stop"}, {"delete"}, {"remove"}, {"clear"}}
	setVerbs    = [][]string{
		{"set"}, {"start"}, {"begin"}, {"make"}, {"create"}, {"add"}, {"put"},
		{"run"}, {"give", "me"}, {"i", "need"}, {"i", "want"},
	}
	// fillers may sit between the anchor and its duration ("timer for ten
	// minutes", "timer on for ten minutes") without breaking the association.
	fillers = map[string]bool{
		"for": true, "of": true, "on": true, "to": true, "at": true,
		"a": true, "an": true, "the": true, "in": true, "please": true,
		"my": true, "another": true,
	}
	// bareWords are the only things allowed to surround a duration-less set
	// request.
	bareWords = map[string]bool{
		"timer": true, "timers": true, "please": true, "now": true,
		"a": true, "an": true, "the": true, "new": true, "another": true, "me": true,
	}
)

func contains(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}

// verbAt reports how many words the matching verb phrase consumed at index i.
func verbAt(words []string, i int, verbs [][]string) int {
	for _, v := range verbs {
		if i+len(v) > len(words) {
			continue
		}
		match := true
		for k, part := range v {
			if words[i+k] != part {
				match = false
				break
			}
		}
		if match {
			return len(v)
		}
	}
	return 0
}

func isQuery(n string, words []string) bool {
	switch {
	case strings.Contains(n, " how long "), strings.Contains(n, " how much "),
		strings.Contains(n, " left "), strings.Contains(n, " remaining "),
		strings.Contains(n, " timers are "):
		return true
	}
	return verbAt(words, 0, [][]string{{"list"}, {"check"}, {"what", "timers"}, {"which", "timers"}}) > 0
}

// bareSetRequest is true for "set a timer", "start another timer please" and
// nothing more: a leading set verb followed only by filler words and the
// anchor itself.
func bareSetRequest(words []string, anchor int) bool {
	used := verbAt(words, 0, setVerbs)
	if used == 0 {
		return false
	}
	for i := used; i < len(words); i++ {
		if i == anchor {
			continue
		}
		if !bareWords[words[i]] {
			return false
		}
	}
	return true
}

// anchoredDuration finds the duration belonging to this request: either the
// phrase ending at the word "timer" ("ten minute timer") or the first phrase
// after it, separated only by fillers ("timer for ten minutes"). A duration
// anywhere else in the sentence belongs to something else and is ignored.
func anchoredDuration(words []string, anchor int) (time.Duration, string, bool) {
	// Longest first, so "twenty five minute timer" reads 25 and not 5.
	for span := 3; span >= 1; span-- {
		if anchor-span < 0 {
			continue
		}
		if d, label, next, ok := phraseAt(words, anchor-span); ok && next == anchor {
			return d, label, true
		}
	}
	// Walk forward past fillers, but try a phrase at each step first: "an" is
	// both a filler and a count, as in "timer for an hour".
	d, label, next := time.Duration(0), "", 0
	found := false
	for i := anchor + 1; i < len(words); i++ {
		if dd, l, nx, ok := phraseAt(words, i); ok {
			d, label, next, found = dd, l, nx, true
			break
		}
		if !fillers[words[i]] {
			break
		}
	}
	if !found {
		return 0, "", false
	}
	// Extend only across an explicit conjunction, so "one hour and thirty
	// minutes" sums while "ten minutes plus wait five minutes" does not.
	for {
		j := next
		if j < len(words) && words[j] == "and" {
			j++
		}
		more, l, nx, good := phraseAt(words, j)
		if !good {
			break
		}
		d += more
		label += " " + l
		next = nx
	}
	return d, label, true
}

// units maps spoken unit words to their duration and canonical singular.
var units = map[string]struct {
	d    time.Duration
	name string
}{
	"second":  {time.Second, "second"},
	"seconds": {time.Second, "second"},
	"sec":     {time.Second, "second"},
	"secs":    {time.Second, "second"},
	"minute":  {time.Minute, "minute"},
	"minutes": {time.Minute, "minute"},
	"min":     {time.Minute, "minute"},
	"mins":    {time.Minute, "minute"},
	"hour":    {time.Hour, "hour"},
	"hours":   {time.Hour, "hour"},
}

var smallWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
	"twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
}

var tensWords = map[string]int{
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60,
	"seventy": 70, "eighty": 80, "ninety": 90,
}

// phraseAt reads "<count> <unit>" starting at index i, accepting a two-word
// count ("twenty five", "half an"). next is the index after the unit.
func phraseAt(words []string, i int) (d time.Duration, label string, next int, ok bool) {
	if i < 0 {
		return 0, "", 0, false
	}
	for _, span := range []int{2, 1} {
		if i+span >= len(words) {
			continue
		}
		u, isUnit := units[words[i+span]]
		if !isUnit {
			continue
		}
		count, spoken, good := countBefore(words[i : i+span])
		if !good {
			continue
		}
		if spoken == "half" {
			return u.d / 2, "half " + article(u.name) + " " + u.name, i + span + 1, true
		}
		return time.Duration(count) * u.d, fmt.Sprintf("%s %s", spoken, u.name), i + span + 1, true
	}
	return 0, "", 0, false
}

func article(unit string) string {
	if unit == "hour" {
		return "an"
	}
	return "a"
}

// countBefore reads the number immediately preceding a unit word, accepting
// digits, "ten", "twenty five", "twenty-five" and "half".
func countBefore(before []string) (int, string, bool) {
	if len(before) == 0 {
		return 0, "", false
	}
	last := before[len(before)-1]
	if last == "half" {
		return 0, "half", true
	}
	// "half an hour", "half a minute": the article sits between.
	if (last == "a" || last == "an") && len(before) >= 2 && before[len(before)-2] == "half" {
		return 0, "half", true
	}
	if v, err := strconv.Atoi(last); err == nil && v > 0 {
		return v, last, true
	}
	if v, ok := smallWords[last]; ok {
		if len(before) >= 2 {
			if tens, ok := tensWords[before[len(before)-2]]; ok && v < 10 {
				return tens + v, before[len(before)-2] + " " + last, true
			}
		}
		spoken := last
		if last == "a" || last == "an" {
			spoken = "one"
		}
		return v, spoken, true
	}
	if v, ok := tensWords[last]; ok {
		return v, last, true
	}
	return 0, "", false
}

// normalise lowercases, drops punctuation, and turns hyphenated numbers into
// separate words so "twenty-five" parses like "twenty five".
func normalise(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Spoken renders a duration the way a person says it, for confirmations and
// announcements.
func Spoken(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	var parts []string
	if h > 0 {
		parts = append(parts, plural(h, "hour"))
	}
	if m > 0 {
		parts = append(parts, plural(m, "minute"))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, plural(s, "second"))
	}
	switch len(parts) {
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
