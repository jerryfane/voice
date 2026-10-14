package brain

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jerryfane/voice/internal/device"
)

// Rules handles deterministic common commands without an LLM round-trip and
// delegates everything else to Next.
type Rules struct {
	Next Planner
	Now  func() time.Time
}

func NewRules(next Planner) *Rules         { return &Rules{Next: next, Now: time.Now} }
func (r *Rules) Name() string              { return "rules -> " + r.Next.Name() }
func (r *Rules) Available() (bool, string) { return r.Next.Available() }
func (r *Rules) Plan(ctx context.Context, text string, inv []device.Info) (Plan, error) {
	n := normalise(text)
	now := r.Now()
	if asksClock(n) {
		return Plan{Speak: fmt.Sprintf("It is %s.", now.Format("3:04 PM")), Source: "rules"}, nil
	}
	if asksDate(n) {
		return Plan{Speak: fmt.Sprintf("It is %s.", now.Format("Monday, January 2")), Source: "rules"}, nil
	}
	if n == "stop" || n == "cancel" || n == "never mind" {
		return Plan{Speak: "Okay.", Source: "rules"}, nil
	}
	if level, ok := musicVolumeCommand(n); ok {
		return musicVolumePlan(inv, level)
	}
	if op, ok := musicCommand(n); ok {
		return musicPlan(inv, op)
	}
	if op, target, ok := phoneQuickCommand(n); ok {
		if d, ok := phoneQuickTarget(n, target, op, inv); ok {
			return actionPlan(d, op, nil)
		}
		return r.Next.Plan(ctx, text, inv)
	}
	// Which devices does this utterance name? Collected rather than acted on
	// immediately, because normalising both sides created a collision the
	// review found: "lamp-2" and "lamp_2" reduce to the same text, and the
	// first match won silently. Acting on the wrong light is worse than the
	// miss that normalising fixed, so an ambiguous name asks instead.
	var named []device.Info
	for _, d := range inv {
		id := normalise(d.ID)
		if id == "" || !strings.Contains(" "+n+" ", " "+id+" ") {
			continue
		}
		named = append(named, d)
	}
	if len(named) > 1 {
		for _, d := range named {
			if d.Kind == "phone" {
				return r.Next.Plan(ctx, text, inv)
			}
		}
		ids := make([]string, 0, len(named))
		for _, d := range named {
			ids = append(ids, d.ID)
		}
		return Plan{
			Speak:  fmt.Sprintf("I know more than one device by that name: %s. Which one?", strings.Join(ids, ", ")),
			Source: "rules",
		}, nil
	}
	for _, d := range named {
		if d.Kind == "climate" {
			if op, args, ok := climateCommand(text, normalise(d.ID)); ok {
				return actionPlan(d, op, args)
			}
		}
		if deviceStatusQuery(n, normalise(d.ID)) {
			return actionPlan(d, device.OpRead, nil)
		}
		if op, args, ok := namedDeviceCommand(n, normalise(d.ID)); ok {
			return actionPlan(d, op, args)
		}
	}
	return r.Next.Plan(ctx, text, inv)
}

// Local writes require a complete command, not a mention of an action.
// Negated, conditional and explanatory requests belong with the planner.
func namedDeviceCommand(text, id string) (device.Op, map[string]any, bool) {
	text = strings.TrimSuffix(strings.TrimPrefix(text, "please "), " please")
	for _, name := range []string{id, "the " + id} {
		switch text {
		case "turn on " + name, "on " + name, "turn " + name + " on", "switch on " + name:
			return device.OpOn, nil, true
		case "turn off " + name, "off " + name, "turn " + name + " off", "switch off " + name:
			return device.OpOff, nil, true
		case "mute " + name:
			return device.OpMute, nil, true
		case "turn " + name + " volume up", "volume up " + name, "make " + name + " louder":
			return device.OpVolumeUp, nil, true
		case "turn " + name + " volume down", "volume down " + name, "make " + name + " quieter":
			return device.OpVolumeDown, nil, true
		}
		for _, color := range []string{"red", "green", "blue", "purple", "orange", "yellow", "cyan", "pink", "white"} {
			if text == "set "+name+" to "+color || text == "make "+name+" "+color || text == "turn "+name+" "+color {
				return device.OpColor, map[string]any{"name": color}, true
			}
		}
	}
	return "", nil, false
}

// deviceStatusQuery is deliberately a complete-utterance match. Mentioning a
// device in a question must never turn a read request into an on/off action.
func deviceStatusQuery(text, id string) bool {
	for _, name := range []string{id, "the " + id} {
		switch text {
		case "status of " + name, "check " + name, "what is " + name + " status",
			"what is " + name + " temperature", "what is the status of " + name,
			"what is the temperature of " + name:
			return true
		}
		for _, state := range []string{"on", "off", "locked", "unlocked"} {
			if text == "is "+name+" "+state {
				return true
			}
		}
	}
	return false
}

func climateCommand(text, id string) (device.Op, map[string]any, bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.TrimSuffix(text, ".")
	split := strings.LastIndex(text, " to ")
	if split < 0 {
		return "", nil, false
	}
	prefix := normalise(text[:split])
	value := strings.TrimSpace(text[split+4:])
	for _, name := range []string{id, "the " + id} {
		switch prefix {
		case "set " + name + " temperature":
			value = strings.TrimSpace(strings.TrimSuffix(value, " degrees"))
			temperature, err := strconv.ParseFloat(value, 64)
			if err == nil && !math.IsNaN(temperature) && !math.IsInf(temperature, 0) {
				return device.OpTemperature, map[string]any{"temperature": temperature}, true
			}
		case "set " + name + " mode":
			mode := strings.ReplaceAll(normalise(value), " ", "_")
			switch mode {
			case "heat", "cool", "off", "auto", "heat_cool", "dry", "fan_only":
				return device.OpHVACMode, map[string]any{"mode": mode}, true
			}
		}
	}
	return "", nil, false
}

// normalise lowercases and removes punctuation, because real transcripts carry
// it and the words are what matter. Whisper produced "Hey boys. What time is
// it?" on the owner's device, and without this the tokens are "it?" and
// "time." - which match neither the filler list nor the subject, so the local
// fast path silently missed every genuine query it was written for. The
// review caught it with the device's own punctuation.
//
// Apostrophes are DELETED rather than turned into spaces, so "what's" becomes
// "whats" - one filler word - instead of "what s", where a stray "s" would
// count as content and disqualify the utterance.
func normalise(text string) string {
	// A rune slice rather than a strings.Builder: the Builder's writers return
	// an error that is always nil, which this repo forbids discarding and
	// which errcheck caught here. Appending needs no error-returning writer.
	out := make([]rune, 0, len(text))
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '\u2019':
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out = append(out, r)
		default:
			out = append(out, ' ')
		}
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// filler holds the words that carry no request of their own, so a clock query
// can be recognised by what is LEFT after removing them.
//
// Substring matching was wrong and the review proved it: "what time" matched
// "what time does the shop close", "the time" matched "the time of the
// meeting", and "what day" matched "what day should I book" - all three
// answered with the current time or date instead of reaching the planner.
// Answering a question nobody asked is worse than the 7 seconds this fast
// path saves, because the user cannot tell they were misunderstood.
var filler = map[string]bool{
	"a": true, "am": true, "and": true, "any": true, "are": true, "can": true,
	"could": true, "currently": true, "current": true, "do": true, "does": true,
	"exactly": true, "for": true, "give": true, "got": true, "have": true,
	"hey": true, "i": true, "is": true, "it": true, "just": true, "know": true,
	"me": true, "my": true, "now": true, "please": true, "right": true,
	"say": true, "so": true, "tell": true, "the": true, "there": true,
	"this": true, "to": true, "today": true, "us": true, "voice": true,
	"was": true, "we": true, "what": true, "whats": true, "when": true,
	"which": true, "you": true, "your": true,
}

// content returns the words that are not filler, which is what decides whether
// an utterance is a bare clock query or a question that happens to mention
// time.
func content(n string) []string {
	var out []string
	for _, w := range strings.Fields(n) {
		if !filler[w] {
			out = append(out, w)
		}
	}
	return out
}

// asksClock reports whether the utterance is ONLY asking for the time. It is
// true for "what time is it", "tell me the time" and "do you know what time it
// is", and false for "what time does the shop close", because that sentence
// carries content words - shop, close - beyond the subject itself.
//
// "timer" is deliberately not a clock word: "set a timer for five minutes" is
// a timer request and the timer path handles it.
func asksClock(n string) bool {
	c := content(n)
	return len(c) == 1 && c[0] == "time"
}

// asksDate applies the same rule to the date.
func asksDate(n string) bool {
	c := content(n)
	return len(c) == 1 && (c[0] == "date" || c[0] == "day")
}

func musicVolumeCommand(n string) (int, bool) {
	fields := strings.Fields(n)
	if len(fields) < 3 {
		return 0, false
	}
	level, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || level < 0 || level > 10 {
		return 0, false
	}
	prefix := strings.Join(fields[:len(fields)-1], " ")
	switch prefix {
	case "music to", "music volume", "music volume to", "set music to",
		"set music volume", "set music volume to", "set the music to",
		"set the music volume", "set the music volume to":
		return level, true
	default:
		return 0, false
	}
}

func musicVolumePlan(inv []device.Info, level int) (Plan, error) {
	var music []device.Info
	for _, d := range inv {
		if d.Kind == "music" {
			music = append(music, d)
		}
	}
	if len(music) == 0 {
		return Plan{Speak: "No music player is configured.", Source: "rules"}, nil
	}
	if len(music) > 1 {
		ids := make([]string, 0, len(music))
		for _, d := range music {
			ids = append(ids, d.ID)
		}
		return Plan{Speak: fmt.Sprintf("Which music player: %s?", strings.Join(ids, ", ")), Source: "rules"}, nil
	}
	p, err := actionPlan(music[0], device.OpVolume, map[string]any{"level": level})
	if err != nil {
		return Plan{}, err
	}
	p.Speak = fmt.Sprintf("Setting the music volume to %d.", level)
	return p, nil
}

func musicCommand(n string) (device.Op, bool) {
	switch n {
	case "play music", "play some music", "play me some music", "start music", "start some music", "put on music", "put some music on":
		return device.OpPlay, true
	case "pause music", "pause the music", "stop music", "stop the music":
		return device.OpPause, true
	case "resume music", "resume the music", "continue music", "continue the music":
		return device.OpResume, true
	case "next song", "next track", "skip song", "skip the song", "skip track", "skip the track":
		return device.OpNext, true
	case "previous song", "previous track", "go back a song", "go back one song":
		return device.OpPrevious, true
	case "turn music up", "turn the music up", "music louder", "raise music volume", "raise the music volume":
		return device.OpVolumeUp, true
	case "turn music down", "turn the music down", "music quieter", "lower music volume", "lower the music volume":
		return device.OpVolumeDown, true
	default:
		return "", false
	}
}

func musicPlan(inv []device.Info, op device.Op) (Plan, error) {
	var music []device.Info
	for _, d := range inv {
		if d.Kind == "music" {
			music = append(music, d)
		}
	}
	if len(music) == 0 {
		return Plan{Speak: "No music player is configured.", Source: "rules"}, nil
	}
	if len(music) > 1 {
		ids := make([]string, 0, len(music))
		for _, d := range music {
			ids = append(ids, d.ID)
		}
		return Plan{Speak: fmt.Sprintf("Which music player: %s?", strings.Join(ids, ", ")), Source: "rules"}, nil
	}
	p, err := actionPlan(music[0], op, nil)
	if err != nil {
		return Plan{}, err
	}
	p.Speak = map[device.Op]string{
		device.OpPlay:       "Playing some music.",
		device.OpPause:      "Pausing the music.",
		device.OpResume:     "Resuming the music.",
		device.OpNext:       "Skipping.",
		device.OpPrevious:   "Going back.",
		device.OpVolumeUp:   "Turning the music up.",
		device.OpVolumeDown: "Turning the music down.",
	}[op]
	return p, nil
}

func actionPlan(d device.Info, op device.Op, args map[string]any) (Plan, error) {
	ok := false
	for _, c := range d.Capabilities {
		if c == string(op) {
			ok = true
			break
		}
	}
	if !ok {
		return Plan{}, fmt.Errorf("%s does not support %s", d.ID, op)
	}
	verb := strings.ReplaceAll(string(op), "_", " ")
	return Plan{Speak: fmt.Sprintf("Okay, %s %s.", verb, d.ID), Actions: []Action{{Device: d.ID, Op: op, Args: args}}, Source: "rules"}, nil
}
