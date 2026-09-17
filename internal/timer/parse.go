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

// Parse recognises the local timer vocabulary. It is deliberately narrow: an
// unrecognised sentence returns None and goes to the planner, which is the
// only safe default when the alternative is silently mishandling a request.
func Parse(text string) Command {
	// n is space-padded so word membership tests cannot match inside a word;
	// head is the trimmed form for leading-verb tests.
	n := normalise(text)
	head := strings.TrimSpace(n)
	if head == "" || !strings.Contains(n, " timer ") && !strings.Contains(n, " timers ") {
		return Command{}
	}
	switch {
	case strings.HasPrefix(head, "cancel"), strings.HasPrefix(head, "stop"),
		strings.HasPrefix(head, "delete"), strings.HasPrefix(head, "remove"):
		c := Command{Kind: Cancel}
		if strings.Contains(n, " all ") || strings.Contains(n, "every timer") || strings.Contains(n, "all timers") {
			c.All = true
			return c
		}
		if d, label, ok := duration(n); ok {
			c.Duration, c.Label = d, label
		}
		return c
	case strings.Contains(n, "how long"), strings.Contains(n, "how much"),
		strings.HasPrefix(head, "list"), strings.HasPrefix(head, "what timers"),
		strings.Contains(n, "timers are"), strings.Contains(n, " left"),
		strings.Contains(n, "remaining"), strings.HasPrefix(head, "check"):
		return Command{Kind: List}
	}
	// Setting requires an explicit request. Without this gate, any sentence
	// mentioning a timer ("why did my timer not go off") would be intercepted
	// and answered with "how long should the timer be?" instead of reaching
	// the planner, which is the one thing this parser must never do.
	if !setIntent(n, head) {
		return Command{}
	}
	if d, label, ok := duration(n); ok {
		return Command{Kind: Set, Duration: d, Label: label}
	}
	// "set a timer" with no duration: ask rather than guess a length.
	return Command{Kind: Set}
}

// setIntent recognises a request to start a timer: a leading verb, or the
// "timer for <duration>" form people use without one.
func setIntent(n, head string) bool {
	for _, verb := range []string{"set ", "start ", "begin ", "make ", "create ", "give me ", "put on ", "run "} {
		if strings.HasPrefix(head, verb) {
			return true
		}
	}
	return strings.Contains(n, " timer for ") || strings.Contains(n, " timer of ")
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

// duration reads one contiguous duration phrase: the first "<count> <unit>"
// in the sentence, extended only while the next words continue it, so
// "one hour thirty minutes" sums but "set a timer for 10 minutes, I have been
// on hold for 20 minutes already" does not. Summing every pair anywhere in the
// utterance silently inflates the timer, which is worse than not parsing it.
func duration(n string) (time.Duration, string, bool) {
	words := strings.Fields(n)
	start, total, label, next, ok := -1, time.Duration(0), "", 0, false
	for i := range words {
		if d, l, nx, good := phraseAt(words, i); good {
			start, total, label, next, ok = i, d, l, nx, true
			break
		}
	}
	if !ok {
		return 0, "", false
	}
	_ = start
	for {
		j := next
		if j < len(words) && (words[j] == "and" || words[j] == "plus") {
			j++
		}
		d, l, nx, good := phraseAt(words, j)
		if !good || j != next && j != next+1 {
			break
		}
		total += d
		label += " " + l
		next = nx
	}
	if total <= 0 {
		return 0, "", false
	}
	return total, label, true
}

// phraseAt reads "<count> <unit>" starting at index i, accepting a two-word
// count ("twenty five", "half an"). next is the index after the unit.
func phraseAt(words []string, i int) (d time.Duration, label string, next int, ok bool) {
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
	return " " + strings.Join(strings.Fields(b.String()), " ") + " "
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
