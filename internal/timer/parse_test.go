package timer

import (
	"testing"
	"time"
)

func TestParseSetsTimersFromSpokenDurations(t *testing.T) {
	for _, tc := range []struct {
		said string
		want time.Duration
	}{
		{"set a timer for ten minutes", 10 * time.Minute},
		{"set a timer for 10 minutes", 10 * time.Minute},
		{"timer for twenty five minutes", 25 * time.Minute},
		{"set a twenty-five minute timer", 25 * time.Minute},
		{"set a timer for one hour", time.Hour},
		{"set a timer for an hour", time.Hour},
		{"set a timer for half an hour", 30 * time.Minute},
		{"set a timer for one hour thirty minutes", 90 * time.Minute},
		{"set a timer for 90 seconds", 90 * time.Second},
		{"start a 3 minute timer", 3 * time.Minute},
		// Bare forms with no leading verb, and verbs the first intent gate
		// missed. All four were rejected by the previous revision.
		{"ten minute timer", 10 * time.Minute},
		{"10 minute timer", 10 * time.Minute},
		{"add a 10 minute timer", 10 * time.Minute},
		{"put a timer on for 10 minutes", 10 * time.Minute},
	} {
		c := Parse(tc.said)
		if c.Kind != Set || c.Duration != tc.want {
			t.Errorf("Parse(%q) = kind %v duration %v, want Set %v", tc.said, c.Kind, c.Duration, tc.want)
		}
	}
}

func TestParseRecognisesCancelAndList(t *testing.T) {
	if c := Parse("cancel the ten minute timer"); c.Kind != Cancel || c.Duration != 10*time.Minute {
		t.Errorf("named cancel = %+v", c)
	}
	if c := Parse("cancel all timers"); c.Kind != Cancel || !c.All {
		t.Errorf("cancel all = %+v", c)
	}
	if c := Parse("cancel the timer"); c.Kind != Cancel || c.All || c.Duration != 0 {
		t.Errorf("bare cancel = %+v", c)
	}
	for _, said := range []string{
		"how long is left on my timer",
		"how much time is left on the timer",
		"what timers are running",
		"list my timers",
	} {
		if c := Parse(said); c.Kind != List {
			t.Errorf("Parse(%q) = %v, want List", said, c.Kind)
		}
	}
}

// Anything that is not a timer request has to reach the planner: silently
// mishandling a request is worse than sending it on. The sentences mentioning
// a timer without asking for one are the dangerous cases - an earlier version
// intercepted them and replied "how long should the timer be?".
func TestParseLeavesOtherSpeechToThePlanner(t *testing.T) {
	for _, said := range []string{
		"turn on the kitchen light",
		"what time is it",
		"how long until dinner",
		"set the living room lamp to ten percent",
		"what is a timer",
		"why did my timer not go off",
		"is the timer working",
		"how does a timer work",
		"my timer app crashed",
		"the timer on the oven is broken",
		// A set verb leading a sentence that is about something else. These
		// were intercepted and answered with "how long should the timer be?".
		"start the timer app",
		"start dinner while the timer runs",
		"begin recording when the timer for the oven goes off",
		"run to the store before the timer for the oven ends",
		"set the timer for the oven aside",
		"give me the remote near the timer for the microwave",
	} {
		if c := Parse(said); c.Kind != None {
			t.Errorf("Parse(%q) = %+v, want None", said, c)
		}
	}
}

// An incidental number elsewhere in the sentence must not be added to the
// timer: a silently inflated timer is worse than an unparsed one.
func TestParseReadsOneContiguousDurationOnly(t *testing.T) {
	for _, tc := range []struct {
		said string
		want time.Duration
	}{
		{"set a timer for 10 minutes i have been on hold for 20 minutes already", 10 * time.Minute},
		{"set a 10 minute timer after this 5 minute video", 10 * time.Minute},
		// A duration mentioned earlier, for something else, must not replace
		// the requested one: the duration has to be anchored to the word
		// "timer", not merely be the first one in the sentence.
		{"i have been on hold 20 minutes set a timer for 10 minutes", 10 * time.Minute},
		{"my break was 20 minutes long set a timer for 10 minutes now", 10 * time.Minute},
		{"the movie is 20 minutes long set a timer for 10 minutes", 10 * time.Minute},
		{"set a timer for 10 minutes plus wait 5 minutes", 10 * time.Minute},
		{"set a timer for one hour thirty minutes", 90 * time.Minute},
		{"set a timer for one hour and thirty minutes", 90 * time.Minute},
	} {
		if c := Parse(tc.said); c.Kind != Set || c.Duration != tc.want {
			t.Errorf("Parse(%q) = kind %v duration %v, want Set %v", tc.said, c.Kind, c.Duration, tc.want)
		}
	}
}

// "set a timer" with no duration must ask, not invent a length.
func TestParseTimerWithoutDurationAsks(t *testing.T) {
	for _, said := range []string{"set a timer", "start a timer please", "set me a new timer"} {
		c := Parse(said)
		if c.Kind != Set || c.Duration != 0 {
			t.Errorf("Parse(%q) = %+v, want Set with no duration", said, c)
		}
	}
}

func TestSpokenReadsLikeSpeech(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{time.Second, "1 second"},
		{90 * time.Second, "1 minute and 30 seconds"},
		{10 * time.Minute, "10 minutes"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "1 hour and 30 minutes"},
		{time.Hour + 30*time.Minute + 15*time.Second, "1 hour, 30 minutes and 15 seconds"},
		{0, "0 seconds"},
	} {
		if got := Spoken(tc.d); got != tc.want {
			t.Errorf("Spoken(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
