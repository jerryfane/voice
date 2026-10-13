// Package timer runs spoken timers locally. Nothing here involves the model:
// a timer must be settable and must still fire when the network, the agent or
// the speech engines are unavailable, and it must survive a restart because a
// kitchen timer that forgets is worse than no timer.
package timer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/voice/internal/faults"
)

// Timer is one scheduled announcement.
type Timer struct {
	ID int `json:"id"`
	// Label is how the user referred to it ("ten minute"), used to address it
	// again and to announce it.
	Label    string        `json:"label"`
	Duration time.Duration `json:"duration"`
	// Deadline is wall-clock, not a countdown: a suspended or slow host must
	// not shift when a timer fires.
	Deadline time.Time `json:"deadline"`
	// Reminder is the spoken message; empty means an ordinary timer.
	Reminder string     `json:"reminder,omitempty"`
	Zone     string     `json:"zone,omitempty"`
	Repeat   Recurrence `json:"repeat,omitempty"`
	// Resume preserves the original recurrence while Deadline is snoozed.
	Resume time.Time `json:"resume,omitempty"`
	// LastDelivered is the deadline of the most recently consumed occurrence,
	// so "last due" remains ordered even when several are delivered together.
	LastDelivered time.Time `json:"last_delivered,omitempty"`
	Archived      bool      `json:"archived,omitempty"`
}

// Remaining is how long is left at now, never negative.
func (t Timer) Remaining(now time.Time) time.Duration {
	if d := t.Deadline.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Saver is how the scheduler persists and recovers its set. Store, backed by
// SQLite, is the production implementation; the interface exists so the
// scheduler can be exercised against a store that fails, which is the only way
// to test that such a failure is reported rather than swallowed.
type Saver interface {
	Load() ([]Timer, error)
	Save([]Timer) error
}

// Scheduler owns the live timer set. It is safe for concurrent use: the
// session loop reads and cancels while the scheduler goroutine fires.
type Scheduler struct {
	store Saver
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	mu         sync.Mutex
	timers     []Timer
	nextID     int
	retryAfter time.Time

	fired   chan Timer
	changed chan struct{}
	faults  faults.Reporter
}

// Option configures a Scheduler. Tests inject a clock; production uses none.
type Option func(*Scheduler)

// WithClock replaces the wall clock and the sleep primitive, so tests can run
// a week of timers without waiting.
func WithClock(now func() time.Time, after func(time.Duration) <-chan time.Time) Option {
	return func(s *Scheduler) { s.now, s.after = now, after }
}

// New loads any persisted timers and returns a scheduler plus the timers that
// expired while Voice was not running, which the caller should report instead
// of firing silently.
// New takes the reporter rather than accepting it as an option: firing happens
// on the scheduler's own goroutine, so every failure there needs somewhere to
// go, and a caller that has not decided where must say faults.Discard out loud.
func New(store Saver, report faults.Reporter, opts ...Option) (*Scheduler, []Timer, error) {
	s := &Scheduler{
		store:   store,
		now:     time.Now,
		after:   time.After,
		fired:   make(chan Timer, 8),
		changed: make(chan struct{}, 1),
		faults:  report,
	}
	for _, o := range opts {
		o(s)
	}
	persisted, err := store.Load()
	if err != nil {
		return s, nil, err
	}
	for _, t := range persisted {
		if t.ID >= s.nextID {
			s.nextID = t.ID + 1
		}
	}
	s.timers = persisted
	sortTimers(s.timers)
	missed, transitionErr := s.consumeDue(s.now())
	if transitionErr != nil {
		s.retryAfter = s.now().Add(time.Second)
	}
	return s, missed, transitionErr
}

func sortTimers(ts []Timer) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Archived != ts[j].Archived {
			return !ts[i].Archived
		}
		if ts[i].Deadline.Equal(ts[j].Deadline) {
			return ts[i].ID < ts[j].ID
		}
		return ts[i].Deadline.Before(ts[j].Deadline)
	})
}

// Fired delivers expired timers to the session loop, which announces them in
// the same place it speaks everything else.
func (s *Scheduler) Fired() <-chan Timer { return s.fired }

// Add schedules a timer and persists the set. Persistence happens under the
// same lock as the mutation, so two writers cannot reorder on the way to disk
// and leave the file describing a set that no longer exists.
func (s *Scheduler) Add(d time.Duration, label string) (Timer, error) {
	return s.add(d, label, "")
}

// AddReminder schedules a one-shot message on the same durable clock as timers.
func (s *Scheduler) AddReminder(d time.Duration, text string) (Timer, error) {
	text = strings.TrimSpace(text)
	if d <= 0 || d > MaxReminderDuration || text == "" {
		return Timer{}, fmt.Errorf("reminders need text and a duration between zero and 365 days")
	}
	return s.add(d, "", text)
}

// ScheduleReminder resolves and persists a complete parsed reminder request.
func (s *Scheduler) ScheduleReminder(c Command) (Timer, error) {
	if c.Schedule == "" {
		return s.AddReminder(c.Duration, c.Text)
	}
	now := s.now()
	when, repeat, err := ResolveSchedule(c.Schedule, now)
	if err != nil {
		return Timer{}, err
	}
	if strings.TrimSpace(c.Text) == "" {
		return Timer{}, fmt.Errorf("say what you want to be reminded about")
	}
	zone := now.Location().String()
	if zone == "Local" {
		zone = hostZone()
	}
	return s.insert(Timer{Duration: when.Sub(now), Deadline: when, Reminder: strings.TrimSpace(c.Text), Repeat: repeat, Zone: zone})
}

func (s *Scheduler) add(d time.Duration, label, reminder string) (Timer, error) {
	return s.insert(Timer{Label: label, Duration: d, Deadline: s.now().Add(d), Reminder: reminder})
}

func (s *Scheduler) insert(t Timer) (Timer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.ID = s.nextID
	next := make([]Timer, len(s.timers)+1)
	copy(next, s.timers)
	next[len(s.timers)] = t
	sortTimers(next)
	if err := s.store.Save(next); err != nil {
		return Timer{}, fmt.Errorf("could not save: %w", err)
	}
	s.timers = next
	s.nextID++
	s.notify()
	return t, nil
}

// List returns the live timers, earliest first.
func (s *Scheduler) List() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var live []Timer
	for _, t := range s.timers {
		if !t.Archived {
			live = append(live, t)
		}
	}
	return live
}

// Reminders includes bounded delivered records so cancelling a completed
// reminder also revokes its snooze eligibility.
func (s *Scheduler) Reminders() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var reminders []Timer
	for _, t := range s.timers {
		if t.Reminder != "" {
			reminders = append(reminders, t)
		}
	}
	return reminders
}

// Snooze replaces the outstanding snooze for one delivered reminder, never
// creates another series, and keeps the original recurrence phase. Occurrences
// falling inside a snooze are coalesced, not replayed afterward.
func (s *Scheduler) Snooze(text string, d time.Duration) (Timer, error) {
	if d <= 0 || d > MaxReminderDuration {
		return Timer{}, fmt.Errorf("use a snooze duration between zero and 365 days")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, t := range s.timers {
		if t.Reminder == "" || t.LastDelivered.IsZero() {
			continue
		}
		if text != "" {
			if !SameReminder(text, t.Reminder) {
				continue
			}
			if index >= 0 {
				return Timer{}, fmt.Errorf("more than one delivered reminder has that text; cancel duplicates before snoozing")
			}
			index = i
		} else if index < 0 || t.LastDelivered.After(s.timers[index].LastDelivered) || t.LastDelivered.Equal(s.timers[index].LastDelivered) && t.ID > s.timers[index].ID {
			index = i
		}
	}
	if index < 0 {
		return Timer{}, fmt.Errorf("there is no matching delivered reminder to snooze")
	}
	next := append([]Timer(nil), s.timers...)
	t := next[index]
	if t.Repeat.Kind != "" && t.Resume.IsZero() {
		t.Resume = t.Deadline
	}
	t.Deadline, t.Archived = s.now().Add(d), false
	next[index] = t
	sortTimers(next)
	if err := s.store.Save(next); err != nil {
		return Timer{}, fmt.Errorf("could not save snooze; the reminder is unchanged: %w", err)
	}
	s.timers = next
	s.notify()
	return t, nil
}

// Cancel removes timers matching a predicate and returns what it removed.
func (s *Scheduler) Cancel(match func(Timer) bool) ([]Timer, error) {
	s.mu.Lock()
	kept := s.timers[:0:0]
	var removed []Timer
	for _, t := range s.timers {
		if match(t) {
			removed = append(removed, t)
			continue
		}
		kept = append(kept, t)
	}
	if len(removed) == 0 {
		s.mu.Unlock()
		return nil, nil
	}
	if err := s.store.Save(kept); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.timers = kept
	s.mu.Unlock()
	s.notify()
	return removed, nil
}

// fail reports an error the scheduler cannot return. Silence here would mean a
// fired timer that reappears after a restart with nobody warned.
func (s *Scheduler) fail(err error) {
	if s.faults != nil {
		s.faults.Report(err)
	}
}

func (s *Scheduler) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Run fires timers until ctx is done. It sleeps until the earliest deadline
// rather than polling, and wakes early whenever the set changes.
func (s *Scheduler) Run(done <-chan struct{}) {
	for {
		wait, have := s.untilNext()
		var alarm <-chan time.Time
		if have {
			alarm = s.after(wait)
		}
		select {
		case <-done:
			return
		case <-s.changed:
			continue
		case <-alarm:
			s.emitDue()
		}
	}
}

func (s *Scheduler) untilNext() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timers) == 0 || s.timers[0].Archived {
		return 0, false
	}
	deadline := s.timers[0].Deadline
	if s.retryAfter.After(deadline) {
		deadline = s.retryAfter
	}
	d := deadline.Sub(s.now())
	if d < 0 {
		d = 0
	}
	return d, true
}

// emitDue moves every timer at or past its deadline out of the live set and
// onto the fired channel. Removal and persistence happen before the send, so a
// crash between firing and announcing loses the announcement, not the state.
func (s *Scheduler) emitDue() {
	now := s.now()
	s.mu.Lock()
	due, err := s.consumeDue(now)
	if err != nil {
		s.retryAfter = now.Add(time.Second)
		s.mu.Unlock()
		s.fail(fmt.Errorf("saving before timers fire; delivery deferred: %w", err))
		return
	}
	s.retryAfter = time.Time{}
	s.mu.Unlock()
	for _, t := range due {
		s.fired <- t
	}
}

// consumeDue is one atomic state transition, also used at startup. Each
// recurring series produces at most one announcement after any downtime.
func (s *Scheduler) consumeDue(now time.Time) ([]Timer, error) {
	if len(s.timers) == 0 || s.timers[0].Archived || s.timers[0].Deadline.After(now) {
		return nil, nil
	}
	var due []Timer
	next := make([]Timer, 0, len(s.timers))
	for _, t := range s.timers {
		if t.Archived || t.Deadline.After(now) {
			next = append(next, t)
			continue
		}
		due = append(due, t)
		if t.Reminder == "" {
			continue
		}
		t.LastDelivered = t.Deadline
		if t.Repeat.Kind == "" {
			t.Archived = true
		} else {
			anchor := t.Deadline
			if !t.Resume.IsZero() {
				anchor = t.Resume
			}
			when, err := t.Repeat.next(anchor, now)
			if err != nil {
				return nil, err
			}
			t.Deadline, t.Resume = when, time.Time{}
		}
		next = append(next, t)
	}
	if len(due) == 0 {
		return nil, nil
	}
	// Only the latest 32 completed one-shot reminders remain snoozable.
	// Active recurring series retain their own last delivery without history.
	sort.SliceStable(next, func(i, j int) bool {
		if next[i].LastDelivered.Equal(next[j].LastDelivered) {
			return next[i].ID > next[j].ID
		}
		return next[i].LastDelivered.After(next[j].LastDelivered)
	})
	kept, archived := next[:0], 0
	for _, t := range next {
		if t.Archived {
			archived++
			if archived > 32 {
				continue
			}
		}
		kept = append(kept, t)
	}
	sortTimers(kept)
	if err := s.store.Save(kept); err != nil {
		return nil, err
	}
	s.timers = kept
	return due, nil
}
