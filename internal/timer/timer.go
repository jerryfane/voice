// Package timer runs spoken timers locally. Nothing here involves the model:
// a timer must be settable and must still fire when the network, the agent or
// the speech engines are unavailable, and it must survive a restart because a
// kitchen timer that forgets is worse than no timer.
package timer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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

// Store persists the timer set as JSON.
type Store struct{ Path string }

// Load reads the persisted timers. A missing file is an empty set. A corrupt
// file is reported and treated as empty, because refusing to start the whole
// assistant over an unreadable timer file would be a worse failure.
func (s Store) Load() ([]Timer, error) {
	if s.Path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ts []Timer
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, fmt.Errorf("%s is not readable timer state: %w", s.Path, err)
	}
	return ts, nil
}

// Save writes the timers atomically: a truncated write during a power cut
// would lose every timer, so the file is replaced by rename.
func (s Store) Save(ts []Timer) error {
	if s.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".timers-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Scheduler owns the live timer set. It is safe for concurrent use: the
// session loop reads and cancels while the scheduler goroutine fires.
type Scheduler struct {
	store Store
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	mu     sync.Mutex
	timers []Timer
	nextID int

	fired   chan Timer
	changed chan struct{}
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
func New(store Store, opts ...Option) (*Scheduler, []Timer, error) {
	s := &Scheduler{
		store:   store,
		now:     time.Now,
		after:   time.After,
		fired:   make(chan Timer, 8),
		changed: make(chan struct{}, 1),
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

// Add schedules a timer and persists the set.
func (s *Scheduler) Add(d time.Duration, label string) (Timer, error) {
	s.mu.Lock()
	t := Timer{ID: s.nextID, Label: label, Duration: d, Deadline: s.now().Add(d)}
	s.nextID++
	s.timers = append(s.timers, t)
	sortTimers(s.timers)
	snapshot := append([]Timer(nil), s.timers...)
	s.mu.Unlock()
	s.notify()
	return t, s.store.Save(snapshot)
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
	snapshot := append([]Timer(nil), s.timers...)
	s.mu.Unlock()
	if len(removed) == 0 {
		return nil, nil
	}
	s.notify()
	return removed, s.store.Save(snapshot)
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
	snapshot := append([]Timer(nil), s.timers...)
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	_ = s.store.Save(snapshot)
	for _, t := range due {
		s.fired <- t
	}
}
