package brain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
)

// The shipped default must answer a clock query on the device, with no model
// round-trip. Asserting the boolean would pass any non-false value while
// proving nothing about routing; this asserts the ROUTING, by failing if the
// planner is reached at all.
//
// It exists because the fast path was built, tested, and shipped disabled: the
// owner's device spent 7.1 seconds asking a remote model what time it was
// while holding the answer in its own clock.
func TestShippedDefaultAnswersTheClockWithoutAModel(t *testing.T) {
	if c := config.Default(); !c.Brain.Rules {
		t.Fatal("the local fast path ships disabled, so trivial queries cost a model call")
	}

	var reached bool
	r := NewRules(&refusingPlanner{onCall: func() { reached = true }})
	r.Now = func() time.Time { return time.Date(2026, 9, 18, 15, 4, 0, 0, time.UTC) }

	for _, q := range []string{
		// As the recogniser actually writes them, punctuation included. The
		// device logged heard="Hey boys. What time is it?", and without
		// punctuation handling the tokens are "it?" and "time." - matching
		// neither the filler list nor the subject, so the fast path missed
		// every real query it was written for.
		"What time is it?",
		"What's the time?",
		"Time.",
		"what time is it",
		"time",
		"what is the time",
		"whats the time",
		"tell me the time",
		"do you know what time it is",
	} {
		p, err := r.Plan(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if reached {
			t.Fatalf("%q reached the planner; a clock query must never leave the device", q)
		}
		if !strings.Contains(p.Speak, "3:04") {
			t.Errorf("%q answered %q, which does not contain the time", q, p.Speak)
		}
		if p.Source != "rules" {
			t.Errorf("%q came from %q, want rules", q, p.Source)
		}
	}

	// The review's false positives: sentences that MENTION time or a day but
	// ask something else entirely. Answering these with the clock is worse
	// than the delay this fast path removes, because the user cannot tell
	// they were misunderstood.
	for _, q := range []string{
		"what time does the shop close",
		"the time of the meeting",
		"what day should I book",
		"what time should we leave",
		"What time does the shop close?",
		"is the date on the letter right",
		"what day is the concert",
	} {
		reached = false
		p, err := r.Plan(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if p.Source == "rules" {
			t.Errorf("%q was answered locally with %q; it asks something the device cannot know", q, p.Speak)
		}
		if !reached {
			t.Errorf("%q never reached the planner", q)
		}
	}

	// A timer request is NOT a clock query: it contains "time" and must
	// still reach the paths that handle it rather than being answered with
	// the current time.
	for _, q := range []string{"set a timer for five minutes", "how long until my timer"} {
		reached = false
		p, err := r.Plan(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if p.Source == "rules" && strings.Contains(p.Speak, "3:04") {
			t.Errorf("%q was answered with the clock: %q", q, p.Speak)
		}
	}

	// A genuine question must still reach the model, or the fast path has
	// become a wall.
	if _, err := r.Plan(context.Background(), "why is the sky blue", nil); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Error("a genuine question did not reach the planner")
	}
}

// refusingPlanner records that it was called, so a test can assert a query
// never left the device.
type refusingPlanner struct{ onCall func() }

func (p *refusingPlanner) Plan(context.Context, string, []device.Info) (Plan, error) {
	p.onCall()
	return Plan{Speak: "from the model"}, nil
}
func (*refusingPlanner) Name() string              { return "refusing" }
func (*refusingPlanner) Available() (bool, string) { return true, "" }

// Device IDs carry punctuation - "living-room", "lamp_2", "hue:desk" - and the
// transcript is now normalised, so the ID must be normalised the same way.
// Comparing normalised text against a raw ID silently broke every punctuated
// device: "turn on the living-room lamp" normalises to "living room" and
// matched nothing.
func TestDeviceIDsWithPunctuationStillMatch(t *testing.T) {
	inv := []device.Info{
		{ID: "living-room", Kind: "light", Capabilities: []string{string(device.OpOn), string(device.OpOff)}},
		{ID: "lamp_2", Kind: "light", Capabilities: []string{string(device.OpOn)}},
		{ID: "hue:desk", Kind: "light", Capabilities: []string{string(device.OpOn)}},
	}
	r := NewRules(&refusingPlanner{onCall: func() {}})

	for _, c := range []struct{ said, want string }{
		{"turn on the living-room", "living-room"},
		{"turn on living room", "living-room"},
		{"turn on lamp_2", "lamp_2"},
		{"turn on lamp 2", "lamp_2"},
		{"turn on hue:desk", "hue:desk"},
	} {
		p, err := r.Plan(context.Background(), c.said, inv)
		if err != nil {
			t.Fatalf("%q: %v", c.said, err)
		}
		if len(p.Actions) == 0 {
			t.Errorf("%q produced no action; the device was not recognised", c.said)
			continue
		}
		if p.Actions[0].Device != c.want {
			t.Errorf("%q acted on %q, want %q", c.said, p.Actions[0].Device, c.want)
		}
	}
}

// Normalising both sides created a collision: "lamp-2" and "lamp_2" reduce to
// the same text, and the first match won silently. Acting on the wrong light
// is worse than the miss that normalising fixed, so an ambiguous name must ask
// rather than choose.
func TestAnAmbiguousDeviceNameAsksInsteadOfGuessing(t *testing.T) {
	inv := []device.Info{
		{ID: "lamp-2", Kind: "light", Capabilities: []string{string(device.OpOn)}},
		{ID: "lamp_2", Kind: "light", Capabilities: []string{string(device.OpOn)}},
	}
	r := NewRules(&refusingPlanner{onCall: func() {}})

	p, err := r.Plan(context.Background(), "turn on lamp 2", inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("acted on %q despite two devices matching; it cannot know which", p.Actions[0].Device)
	}
	for _, want := range []string{"lamp-2", "lamp_2"} {
		if !strings.Contains(p.Speak, want) {
			t.Errorf("the question does not name %q, so the user cannot answer it: %q", want, p.Speak)
		}
	}

	// One matching device still acts, or the guard has become a wall.
	single := []device.Info{{ID: "lamp-2", Kind: "light", Capabilities: []string{string(device.OpOn)}}}
	p, err = r.Plan(context.Background(), "turn on lamp 2", single)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 1 || p.Actions[0].Device != "lamp-2" {
		t.Errorf("a single match must still act, got %+v", p.Actions)
	}
}
