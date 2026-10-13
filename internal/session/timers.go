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
// here or not at all. stopThinking must finish before any timer reply speaks.
func (a *Assistant) timerControl(ctx context.Context, command string, stopThinking func()) (handled, spoken bool) {
	c := timer.Parse(command)
	if a.Timers == nil {
		if c.Reminder {
			stopThinking()
			return true, a.say(ctx, "Reminders are unavailable because the local scheduler is disabled.")
		}
		return false, false
	}
	if c.Kind == timer.None {
		return false, false
	}
	stopThinking()
	if c.Reminder {
		return true, a.reminderControl(ctx, c)
	}
	switch c.Kind {
	case timer.Set:
		if c.Duration <= 0 {
			return true, a.say(ctx, "How long should the timer be?")
		}
		t, err := a.Timers.Add(c.Duration, c.Label)
		if err != nil {
			// The timer is live; only its persistence failed, which matters
			// on restart and so is worth saying out loud.
			a.logf("timer persist: %v", err)
			return true, a.say(ctx, fmt.Sprintf("%s timer started, but I could not save it, so a restart will lose it.", timer.Spoken(t.Duration)))
		}
		a.logf("timer set id=%d label=%q duration=%s", t.ID, t.Label, t.Duration)
		return true, a.say(ctx, fmt.Sprintf("%s timer started.", timer.Spoken(t.Duration)))
	case timer.Cancel:
		return true, a.say(ctx, a.cancelTimers(c))
	case timer.List:
		return true, a.say(ctx, a.describeTimers())
	}
	return false, false
}

func (a *Assistant) cancelTimers(c timer.Command) string {
	live := a.scheduled(false)
	if len(live) == 0 {
		return "There are no timers running."
	}
	match := func(t timer.Timer) bool { return t.Reminder == "" }
	switch {
	case c.All, len(live) == 1 && c.Duration == 0:
		// "cancel the timer" with exactly one running is unambiguous.
	case c.Duration > 0:
		match = func(t timer.Timer) bool { return t.Reminder == "" && t.Duration == c.Duration }
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
	live := a.scheduled(false)
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
		if t.Reminder != "" {
			a.say(ctx, fmt.Sprintf("While I was off, your reminder to %s was due %s ago.", t.Reminder, timer.Spoken(ago)))
			continue
		}
		a.say(ctx, fmt.Sprintf("Your %s timer expired %s ago, while I was off.",
			timer.Spoken(t.Duration), timer.Spoken(ago)))
	}
}

func (a *Assistant) announceFired(ctx context.Context, t timer.Timer) bool {
	a.logf("timer fired id=%d label=%q", t.ID, t.Label)
	if t.Reminder != "" {
		if late := a.now().Sub(t.Deadline); late >= time.Minute {
			return a.say(ctx, fmt.Sprintf("Your reminder was due %s ago: %s", timer.Spoken(late), t.Reminder))
		}
		return a.say(ctx, "Reminder: "+t.Reminder)
	}
	return a.say(ctx, fmt.Sprintf("Your %s timer is done.", timer.Spoken(t.Duration)))
}

// scheduled separates reminder vocabulary from timer vocabulary without a
// second scheduler. "Cancel all timers" must not erase personal reminders.
func (a *Assistant) scheduled(reminders bool) []timer.Timer {
	live := a.Timers.List()
	kept := live[:0]
	for _, t := range live {
		if (t.Reminder != "") == reminders {
			kept = append(kept, t)
		}
	}
	return kept
}

func (a *Assistant) reminderControl(ctx context.Context, c timer.Command) bool {
	switch c.Kind {
	case timer.Set:
		if c.Duration <= 0 || c.Duration > timer.MaxReminderDuration || c.Text == "" {
			return a.say(ctx, "Say remind me in ten minutes to call Sam. Use a positive duration in seconds, minutes, hours, days, or weeks, up to 365 days.")
		}
		t, err := a.Timers.AddReminder(c.Duration, c.Text)
		if err != nil {
			a.logf("reminder persist: %v", err)
			return a.say(ctx, fmt.Sprintf("I'll remind you in %s to %s, but I could not save it, so a restart will lose it.", timer.Spoken(t.Duration), t.Reminder))
		}
		return a.say(ctx, fmt.Sprintf("I'll remind you in %s to %s.", timer.Spoken(t.Duration), t.Reminder))
	case timer.List:
		live := a.scheduled(true)
		if len(live) == 0 {
			return a.say(ctx, "You have no pending reminders.")
		}
		now := a.now()
		parts := make([]string, 0, len(live))
		for _, t := range live {
			if !t.Deadline.After(now) {
				parts = append(parts, fmt.Sprintf("Overdue by %s: %s", timer.Spoken(now.Sub(t.Deadline)), t.Reminder))
			} else {
				parts = append(parts, fmt.Sprintf("In %s: %s", timer.Spoken(t.Remaining(now)), t.Reminder))
			}
		}
		return a.say(ctx, strings.Join(parts, ". ")+".")
	case timer.Cancel:
		return a.say(ctx, a.cancelReminders(c))
	}
	return false
}

func (a *Assistant) cancelReminders(c timer.Command) string {
	live := a.scheduled(true)
	if len(live) == 0 {
		return "You have no pending reminders."
	}
	var matches []timer.Timer
	for _, t := range live {
		if c.All || c.Text == "" || timer.SameReminder(t.Reminder, c.Text) {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return "I don't have a reminder with that exact text."
	}
	if !c.All && len(matches) > 1 {
		return "There is more than one reminder. Say cancel the reminder to, followed by its full text, or cancel all reminders."
	}
	removed, err := a.Timers.Cancel(func(t timer.Timer) bool {
		return t.Reminder != "" && (c.All || t.ID == matches[0].ID)
	})
	if err != nil {
		a.logf("reminder persist: %v", err)
		return "Cancelled the reminder, but I could not save that, so a restart may bring it back."
	}
	if len(removed) == 0 {
		return "That reminder is no longer pending."
	}
	if len(removed) == 1 {
		return "Cancelled your reminder to " + removed[0].Reminder + "."
	}
	return fmt.Sprintf("Cancelled %d reminders.", len(removed))
}

func (a *Assistant) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// say speaks and logs, for the paths where a speech failure must not abort the
// work that produced the words. It routes through speakLogged so timer
// prompts, confirmations and announcements are timed like every other spoken
// reply: this was a SECOND entry point to Speak, and while it existed the
// claim that every callsite was timed was false - eight production callsites
// stayed invisible, including the planner-failure fallback.
func (a *Assistant) say(ctx context.Context, text string) bool {
	return a.speakLogged(ctx, text)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
