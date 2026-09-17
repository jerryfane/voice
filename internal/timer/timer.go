// Package timer runs spoken timers locally. Nothing here involves the model:
// a timer must be settable and must still fire when the network, the agent or
// the speech engines are unavailable, and it must survive a restart because a
// kitchen timer that forgets is worse than no timer.
package timer

import (
	"fmt"
	"sort"
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

	mu     sync.Mutex
	timers []Timer
	nextID int

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
	now := s.now()
	var missed []Timer
	for _, t := range persisted {
		if t.ID >= s.nextID {
			s.nextID = t.ID + 1
		}
		if t.Deadline.After(now) {
			s.timers = append(s.timers, t)
		} else {
			missed = append(missed, t)
		}
	}
	sortTimers(s.timers)
	if len(missed) > 0 {
		// The missed ones are gone from the live set, so persist immediately;
		// a restart loop must not announce them twice.
		if saveErr := store.Save(s.timers); saveErr != nil && err == nil {
			err = saveErr
		}
	}
	return s, missed, err
}

func sortTimers(ts []Timer) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Deadline.Before(ts[j].Deadline) })
}

// Fired delivers expired timers to the session loop, which announces them in
// the same place it speaks everything else.
func (s *Scheduler) Fired() <-chan Timer { return s.fired }

// Add schedules a timer and persists the set. Persistence happens under the
// same lock as the mutation, so two writers cannot reorder on the way to disk
// and leave the file describing a set that no longer exists.
func (s *Scheduler) Add(d time.Duration, label string) (Timer, error) {
	s.mu.Lock()
	t := Timer{ID: s.nextID, Label: label, Duration: d, Deadline: s.now().Add(d)}
	s.nextID++
	s.timers = append(s.timers, t)
	sortTimers(s.timers)
	err := s.store.Save(s.timers)
	s.mu.Unlock()
	s.notify()
	return t, err
}

// List returns the live timers, earliest first.
func (s *Scheduler) List() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Timer(nil), s.timers...)
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
	s.timers = kept
	var err error
	if len(removed) > 0 {
		err = s.store.Save(s.timers)
	}
	s.mu.Unlock()
	if len(removed) == 0 {
		return nil, nil
	}
	s.notify()
	return removed, err
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
	if len(s.timers) == 0 {
		return 0, false
	}
	d := s.timers[0].Deadline.Sub(s.now())
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
	var due []Timer
	kept := s.timers[:0:0]
	for _, t := range s.timers {
		if !t.Deadline.After(now) {
			due = append(due, t)
			continue
		}
		kept = append(kept, t)
	}
	s.timers = kept
	if len(due) > 0 {
		// Persisting before the send means a crash between firing and
		// announcing loses the announcement, not the state.
		if err := s.store.Save(s.timers); err != nil {
			s.fail(fmt.Errorf("saving after %d timer(s) fired: %w", len(due), err))
		}
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	for _, t := range due {
		s.fired <- t
	}
}
