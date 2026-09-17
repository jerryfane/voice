package timer

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/faults"
)

// fakeClock drives the scheduler without waiting. Advance releases every
// sleeper whose deadline has passed, which is what lets a test cover a
// multi-hour timer in microseconds.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	// A sleeper registered for a deadline that has already passed fires at
	// once; otherwise a scheduler that registers just after an Advance would
	// wait forever for a wake-up that already happened.
	if !at.After(c.now) {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: at, ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []waiter
	kept := c.waiters[:0:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			due = append(due, w)
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
	now := c.now
	c.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// scheduler starts a scheduler on the fake clock and returns an idempotent
// stop function, so a test can end the "process" early and still let cleanup
// run.
// store opens the real SQLite store and closes it with the test, so the
// persistence path under test is the one production uses.
func store(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	return st
}

func scheduler(t *testing.T, path string, c *fakeClock) (*Scheduler, []Timer, func()) {
	t.Helper()
	s, missed, err := New(store(t, path), faults.Discard, WithClock(c.Now, c.After))
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	go s.Run(done)
	t.Cleanup(stop)
	return s, missed, stop
}

// waitFired waits for one fired timer, sweeping the clock so a sleeper the
// scheduler registered after the test advanced time still wakes.
func waitFired(t *testing.T, s *Scheduler, c *fakeClock) Timer {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case fired := <-s.Fired():
			return fired
		case <-time.After(5 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("timer never fired")
			}
			c.Advance(0)
		}
	}
}

func TestTimerFiresAtItsWallClockDeadline(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "timers.db")
	s, _, _ := scheduler(t, path, c)

	if _, err := s.Add(10*time.Minute, "ten minute"); err != nil {
		t.Fatal(err)
	}
	c.Advance(9 * time.Minute)
	select {
	case fired := <-s.Fired():
		t.Fatalf("timer fired early: %+v", fired)
	case <-time.After(20 * time.Millisecond):
	}
	c.Advance(time.Minute)
	if fired := waitFired(t, s, c); fired.Duration != 10*time.Minute {
		t.Errorf("fired %+v, want the ten minute timer", fired)
	}
	if live := s.List(); len(live) != 0 {
		t.Errorf("%d timers left after firing, want 0", len(live))
	}
}

func TestConcurrentTimersFireInDeadlineOrder(t *testing.T) {
	c := newClock()
	s, _, _ := scheduler(t, filepath.Join(t.TempDir(), "timers.db"), c)

	for _, d := range []time.Duration{30 * time.Minute, 5 * time.Minute, time.Hour} {
		if _, err := s.Add(d, Spoken(d)); err != nil {
			t.Fatal(err)
		}
	}
	if live := s.List(); len(live) != 3 || live[0].Duration != 5*time.Minute {
		t.Fatalf("live set = %+v, want three timers earliest first", live)
	}
	var order []time.Duration
	for range 3 {
		c.Advance(time.Hour)
		order = append(order, waitFired(t, s, c).Duration)
	}
	want := []time.Duration{5 * time.Minute, 30 * time.Minute, time.Hour}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("fired order %v, want %v", order, want)
		}
	}
}

func TestCancelRemovesOnlyTheNamedTimer(t *testing.T) {
	c := newClock()
	s, _, _ := scheduler(t, filepath.Join(t.TempDir(), "timers.db"), c)
	for _, d := range []time.Duration{5 * time.Minute, 20 * time.Minute} {
		if _, err := s.Add(d, Spoken(d)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.Cancel(func(x Timer) bool { return x.Duration == 5*time.Minute })
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Duration != 5*time.Minute {
		t.Fatalf("removed %+v, want the five minute timer", removed)
	}
	c.Advance(10 * time.Minute)
	select {
	case fired := <-s.Fired():
		t.Fatalf("cancelled timer fired: %+v", fired)
	case <-time.After(20 * time.Millisecond):
	}
	c.Advance(15 * time.Minute)
	if fired := waitFired(t, s, c); fired.Duration != 20*time.Minute {
		t.Errorf("fired %+v, want the twenty minute timer", fired)
	}
}

// A timer set before a restart must still fire at its original wall-clock
// time, with the remaining time it actually has left, not the time it was set
// for.
func TestTimerSurvivesRestartWithCorrectRemaining(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	first := newClock()
	s, _, stop := scheduler(t, path, first)
	if _, err := s.Add(30*time.Minute, "thirty minute"); err != nil {
		t.Fatal(err)
	}
	stop() // the process stops here

	restart := newClock()
	restart.Advance(10 * time.Minute) // ten minutes of downtime
	s2, missed, _ := scheduler(t, path, restart)
	if len(missed) != 0 {
		t.Fatalf("reported %d missed timers, want none", len(missed))
	}
	live := s2.List()
	if len(live) != 1 {
		t.Fatalf("%d timers after restart, want 1", len(live))
	}
	if got := live[0].Remaining(restart.Now()); got != 20*time.Minute {
		t.Errorf("remaining after restart = %v, want 20m", got)
	}
	restart.Advance(20 * time.Minute)
	if fired := waitFired(t, s2, restart); fired.Duration != 30*time.Minute {
		t.Errorf("fired %+v, want the thirty minute timer", fired)
	}
}

// A timer that came due while Voice was down must be reported as expired, not
// fired as if it had just finished, and must not be reported twice.
func TestTimerExpiredWhileDownIsReportedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	first := newClock()
	s, _, stop := scheduler(t, path, first)
	if _, err := s.Add(5*time.Minute, "five minute"); err != nil {
		t.Fatal(err)
	}
	stop()

	restart := newClock()
	restart.Advance(9 * time.Minute)
	s2, missed, stop2 := scheduler(t, path, restart)
	if len(missed) != 1 || missed[0].Duration != 5*time.Minute {
		t.Fatalf("missed = %+v, want the five minute timer", missed)
	}
	if live := s2.List(); len(live) != 0 {
		t.Errorf("expired timer still live: %+v", live)
	}
	select {
	case fired := <-s2.Fired():
		t.Fatalf("expired-while-down timer also fired: %+v", fired)
	case <-time.After(20 * time.Millisecond):
	}
	stop2()

	again := newClock()
	again.Advance(20 * time.Minute)
	_, missedAgain, _ := scheduler(t, path, again)
	if len(missedAgain) != 0 {
		t.Errorf("second restart reported %+v again", missedAgain)
	}
}

// A state file from the pre-release JSON store must be refused with a message
// that says what to do, not silently discarded or half-parsed.
func TestLegacyJSONStateIsRejectedWithAClearMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	if err := os.WriteFile(path, []byte(`[{"id":1,"duration":`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newClock()
	st, err := Open(path)
	if err == nil {
		t.Fatal("JSON state from a pre-release build was accepted silently")
	}
	if !strings.Contains(err.Error(), "JSON timer state") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
	// With the stale file removed, the same path works.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	st = store(t, path)
	s, _, err := New(st, faults.Discard, WithClock(c.Now, c.After))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go s.Run(done)
	if _, err := s.Add(time.Minute, "one minute"); err != nil {
		t.Fatalf("cannot set a timer after rejecting stale state: %v", err)
	}
	c.Advance(time.Minute)
	if fired := waitFired(t, s, c); fired.Duration != time.Minute {
		t.Errorf("fired %+v after recovery", fired)
	}
}

// Saving replaces the whole set in one transaction and leaves no journal or
// temporary file behind once the store is closed.
func TestSaveReplacesTheStoredSetTransactionally(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timers.db")
	st := store(t, path)
	if err := st.Save([]Timer{{ID: 1, Duration: time.Minute, Deadline: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("load after empty save = %+v, %v", loaded, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "timers.db" {
			t.Errorf("left %q behind in the state directory", e.Name())
		}
	}
}

// A database that is not a database must disable timers with an explanation,
// not panic and not pretend the set is empty.
func TestUnreadableDatabaseIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	if err := os.WriteFile(path, []byte("this is not a database at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a corrupt database was accepted")
	}
}

// Persisted state must always describe the live set: a concurrent add and
// cancel storm must not leave the file describing timers that no longer exist,
// or missing ones that do.
func TestPersistedStateMatchesLiveSetUnderConcurrency(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "timers.db")
	s, _, _ := scheduler(t, path, c)

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := time.Duration(i+1) * time.Minute
			if _, err := s.Add(d, Spoken(d)); err != nil {
				t.Error(err)
			}
			if i%2 == 0 {
				if _, err := s.Cancel(func(x Timer) bool { return x.Duration == d }); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()

	live := s.List()
	persisted, err := store(t, path).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(persisted) != len(live) {
		t.Fatalf("persisted %d timers, live set has %d", len(persisted), len(live))
	}
	liveIDs := map[int]bool{}
	for _, x := range live {
		liveIDs[x.ID] = true
	}
	for _, p := range persisted {
		if !liveIDs[p.ID] {
			t.Errorf("persisted timer %d (%v) is not live", p.ID, p.Duration)
		}
	}
}

// A failure to persist after a timer fires has nowhere to return, so it must
// be reported: silence there means a fired timer can reappear after a restart
// with nobody warned.
func TestSaveFailureAfterFiringIsReported(t *testing.T) {
	c := newClock()
	var mu sync.Mutex
	var errs []error
	store := &flakyStore{}
	report := faults.Func(func(e error) { mu.Lock(); errs = append(errs, e); mu.Unlock() })
	s, _, err := New(store, report, WithClock(c.Now, c.After))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go s.Run(done)
	if _, err := s.Add(time.Minute, "one minute"); err != nil {
		t.Fatal(err)
	}
	store.failFrom(errors.New("disk full"))

	c.Advance(time.Minute)
	if fired := waitFired(t, s, c); fired.Duration != time.Minute {
		t.Fatalf("fired %+v", fired)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("save failure after firing was not reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// flakyStore starts healthy and fails every save after failFrom is called.
type flakyStore struct {
	mu    sync.Mutex
	set   []Timer
	fails error
}

func (f *flakyStore) failFrom(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = err
}

func (f *flakyStore) Load() ([]Timer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Timer(nil), f.set...), nil
}

func (f *flakyStore) Save(ts []Timer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails != nil {
		return f.fails
	}
	f.set = append([]Timer(nil), ts...)
	return nil
}

// A JSON state file with leading whitespace is still a JSON state file. It
// used to reach SQLite and come back as "file is not a database", which tells
// the operator nothing about what to do.
func TestLegacyStateIsRecognisedThroughLeadingWhitespace(t *testing.T) {
	for _, body := range []string{"[]", "\n  {\"timers\":[]}", "\t\r\n[{\"id\":1}]"} {
		path := filepath.Join(t.TempDir(), "timers.db")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Open(path)
		if err == nil {
			t.Errorf("%q accepted as a database", body)
			continue
		}
		if !strings.Contains(err.Error(), "JSON timer state") {
			t.Errorf("%q reported as %v, want the pre-release JSON message", body, err)
		}
	}
}

// A mistyped timers.file pointing at somebody else's database must be refused,
// not quietly given a timers table.
func TestForeignDatabaseIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	other, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT); INSERT INTO notes (body) VALUES ('keep me')`); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a foreign database was accepted")
	} else if !strings.Contains(err.Error(), "timers.file") {
		t.Errorf("error does not point at the misconfiguration: %v", err)
	}
	// The other database is untouched.
	again, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var tables int
	if err := again.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Errorf("%d tables after a refused open, want the original 1", tables)
	}
}

// Reopening an existing timers database must not write to it: `voice doctor`
// runs while the service holds the file, and a write on every open turned an
// inspection into lock contention.
func TestReopeningAnExistingDatabaseDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	first := store(t, path)
	if err := first.Save([]Timer{{ID: 1, Label: "one minute", Duration: time.Minute, Deadline: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	second := store(t, path)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("opening an existing database modified it: %v/%d -> %v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	loaded, err := second.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("second opener loaded %+v, %v", loaded, err)
	}
}
