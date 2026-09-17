package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/timer"
)

// timerControl handles the local timer vocabulary before the planner sees the
// command. Timers must work with no model and no network, so they are answered
// here or not at all.
func (a *Assistant) timerControl(ctx context.Context, command string) (handled bool) {
	if a.Timers == nil {
		return false
	}
	c := timer.Parse(command)
	switch c.Kind {
	case timer.None:
		return false
	case timer.Set:
		if c.Duration <= 0 {
			a.say(ctx, "How long should the timer be?")
			return true
		}
		t, err := a.Timers.Add(c.Duration, c.Label)
		if err != nil {
			// The timer is live; only its persistence failed, which matters
			// on restart and so is worth saying out loud.
			a.logf("timer persist: %v", err)
			a.say(ctx, fmt.Sprintf("%s timer started, but I could not save it, so a restart will lose it.", timer.Spoken(t.Duration)))
			return true
		}
		a.logf("timer set id=%d label=%q duration=%s", t.ID, t.Label, t.Duration)
		a.say(ctx, fmt.Sprintf("%s timer started.", timer.Spoken(t.Duration)))
		return true
	case timer.Cancel:
		a.say(ctx, a.cancelTimers(c))
		return true
	case timer.List:
		a.say(ctx, a.describeTimers())
		return true
	}
	return false
}

func (a *Assistant) cancelTimers(c timer.Command) string {
	live := a.Timers.List()
	if len(live) == 0 {
		return "There are no timers running."
	}
	match := func(timer.Timer) bool { return true }
	switch {
	case c.All, len(live) == 1 && c.Duration == 0:
		// "cancel the timer" with exactly one running is unambiguous.
	case c.Duration > 0:
		match = func(t timer.Timer) bool { return t.Duration == c.Duration }
	default:
		return fmt.Sprintf("There are %d timers. Say which one, for example cancel the %s timer.",
			len(live), timer.Spoken(live[0].Duration))
	}
	removed, err := a.Timers.Cancel(match)
	switch {
	case len(removed) == 0:
		return "I don't have a timer for that."
	case err != nil:
		// The timer is gone from the live set but the file still lists it, so
		// a restart would bring it back. The user has to hear that, for the
		// same reason a failed save is reported when setting one.
		a.logf("timer persist: %v", err)
		return fmt.Sprintf("Cancelled %d timer(s), but I could not save that, so a restart may bring them back.", len(removed))
	case len(removed) == 1:
		return fmt.Sprintf("%s timer cancelled.", timer.Spoken(removed[0].Duration))
	default:
		return fmt.Sprintf("Cancelled %d timers.", len(removed))
	}
}

func (a *Assistant) describeTimers() string {
	live := a.Timers.List()
	if len(live) == 0 {
		return "There are no timers running."
	}
	now := a.now()
	parts := make([]string, 0, len(live))
	for _, t := range live {
		parts = append(parts, fmt.Sprintf("%s left on the %s timer", timer.Spoken(t.Remaining(now)), timer.Spoken(t.Duration)))
	}
	if len(parts) == 1 {
		return upperFirst(parts[0]) + "."
	}
	return upperFirst(strings.Join(parts[:len(parts)-1], ", ")) + ", and " + parts[len(parts)-1] + "."
}

// announceMissed reports timers that came due while Voice was not running.
// Firing them silently would be a lie, and firing them as if they just expired
// would be worse.
func (a *Assistant) announceMissed(ctx context.Context, missed []timer.Timer) {
	now := a.now()
	for _, t := range missed {
		ago := now.Sub(t.Deadline)
		a.logf("timer expired while offline id=%d label=%q ago=%s", t.ID, t.Label, ago)
		a.say(ctx, fmt.Sprintf("Your %s timer expired %s ago, while I was off.",
			timer.Spoken(t.Duration), timer.Spoken(ago)))
	}
}

func (a *Assistant) announceFired(ctx context.Context, t timer.Timer) {
	a.logf("timer fired id=%d label=%q", t.ID, t.Label)
	a.say(ctx, fmt.Sprintf("Your %s timer is done.", timer.Spoken(t.Duration)))
}

func (a *Assistant) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// say speaks and logs, for the paths where a speech failure must not abort the
// work that produced the words.
func (a *Assistant) say(ctx context.Context, text string) {
	if err := a.Speak(ctx, text); err != nil {
		a.logf("speak: %v", err)
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
