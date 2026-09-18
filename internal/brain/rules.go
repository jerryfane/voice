package brain

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	n := strings.ToLower(strings.TrimSpace(text))
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
	for _, d := range inv {
		id := strings.ToLower(d.ID)
		if !strings.Contains(n, id) {
			continue
		}
		if strings.Contains(n, "turn on") || strings.HasPrefix(n, "on ") {
			return actionPlan(d, device.OpOn, nil)
		}
		if strings.Contains(n, "turn off") || strings.HasPrefix(n, "off ") {
			return actionPlan(d, device.OpOff, nil)
		}
		if strings.Contains(n, "mute") {
			return actionPlan(d, device.OpMute, nil)
		}
		if strings.Contains(n, "volume up") || strings.Contains(n, "louder") {
			return actionPlan(d, device.OpVolumeUp, nil)
		}
		if strings.Contains(n, "volume down") || strings.Contains(n, "quieter") {
			return actionPlan(d, device.OpVolumeDown, nil)
		}
		for _, c := range []string{"red", "green", "blue", "purple", "orange", "yellow", "cyan", "pink", "white"} {
			if strings.Contains(n, c) {
				return actionPlan(d, device.OpColor, map[string]any{"name": c})
			}
		}
	}
	return r.Next.Plan(ctx, text, inv)
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
