package main

import (
	"errors"
	"testing"

	"github.com/jerryfane/voice/internal/config"
)

// A non-match must be a FAILING exit status, because the whole point of this
// command is to be asserted after an install - by a person or a script - and
// a checker that exits 0 whatever it finds cannot fail a bad deploy.
func TestWakeCheckExitStatusDistinguishesMatchFromNoMatch(t *testing.T) {
	c := config.Default()
	c.Wake.Phrases = []string{"hey voice", "hey boys"}
	c.Wake.Fuzz = 0

	for _, spoken := range []string{"hey voice", "hey boys", "hey boys turn on the lights"} {
		if err := checkWake(c, spoken); err != nil {
			t.Errorf("checkWake(%q) = %v, want success", spoken, err)
		}
	}

	if err := checkWake(c, "hey neighbours"); !errors.Is(err, errWakeNoMatch) {
		t.Errorf("checkWake on an unconfigured phrase = %v, want errWakeNoMatch so a deploy check can fail", err)
	}
}
