package timer

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/faults"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestCalendarVocabularyAndHostZone(t *testing.T) {
	loc := zone(t, "Asia/Dubai")
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, loc)
	for _, tc := range []struct {
		schedule, want string
		kind           string
	}{
		{"tomorrow at 9", "2026-10-14T09:00:00+04:00", ""},
		{"tomorrow at nine", "2026-10-14T09:00:00+04:00", ""},
		{"tomorrow at 9 a.m.", "2026-10-14T09:00:00+04:00", ""},
		{"tomorrow at 9 pm", "2026-10-14T21:00:00+04:00", ""},
		{"today at 12 pm", "", ""},
		{"today at 12:01", "2026-10-13T12:01:00+04:00", ""},
		{"next Tuesday at 9", "2026-10-20T09:00:00+04:00", ""},
		{"next Monday at 9", "2026-10-19T09:00:00+04:00", ""},
		{"on 2026-12-25 at 00:30", "2026-12-25T00:30:00+04:00", ""},
		{"2026-12-25 at twelve a.m.", "2026-12-25T00:00:00+04:00", ""},
		{"every day at 9", "2026-10-14T09:00:00+04:00", "daily"},
		{"daily at 21", "2026-10-13T21:00:00+04:00", "daily"},
		{"every Monday at nine", "2026-10-19T09:00:00+04:00", "weekly"},
		{"weekly on Tuesday at 13", "2026-10-13T13:00:00+04:00", "weekly"},
		{"every two hours", "2026-10-13T14:00:00+04:00", "interval"},
	} {
		t.Run(tc.schedule, func(t *testing.T) {
			c := Parse("remind me " + tc.schedule + " to Call Élodie!")
			if c.Kind != Set || !c.Reminder || c.Text != "Call Élodie!" {
				t.Fatalf("parse: %+v", c)
			}
			when, repeat, err := ResolveSchedule(c.Schedule, now)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted past today")
				}
				return
			}
			if err != nil || when.Format(time.RFC3339) != tc.want || repeat.Kind != tc.kind {
				t.Fatalf("got %v %+v %v, want %s %s", when, repeat, err, tc.want, tc.kind)
			}
		})
	}
	for _, schedule := range []string{"today at 9", "on 2026-02-30 at 9", "tomorrow at 24", "tomorrow at 9:60", "tomorrow at 0 pm", "tomorrow at 13 am", "tomorrow at -9", "tomorrow at 1.5", "on 2028-01-01 at 9", "every Friday", "every zero minutes", "every 366 days"} {
		if _, _, err := ResolveSchedule(schedule, now); err == nil {
			t.Errorf("accepted %q", schedule)
		}
	}
}

func TestCalendarDSTGapFoldAndWeeklyWallClock(t *testing.T) {
	loc := zone(t, "America/New_York")
	beforeGap := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	if _, _, err := ResolveSchedule("tomorrow at 2:30", beforeGap); err == nil {
		t.Fatal("accepted nonexistent wall time")
	}
	first, daily, err := ResolveSchedule("every day at 2:30", beforeGap)
	if err != nil || first.Format(time.RFC3339) != "2026-03-09T02:30:00-04:00" {
		t.Fatalf("gap skip = %v %v", first, err)
	}
	if daily.Zone != "America/New_York" {
		t.Fatalf("lost zone: %+v", daily)
	}
	beforeFold := time.Date(2026, 10, 31, 12, 0, 0, 0, loc)
	fold, daily, err := ResolveSchedule("every day at 1:30", beforeFold)
	if err != nil || fold.Format(time.RFC3339) != "2026-11-01T01:30:00-04:00" {
		t.Fatalf("fold = %v %v", fold, err)
	}
	next, err := daily.next(fold, fold.Add(30*time.Minute))
	if err != nil || next.Format(time.RFC3339) != "2026-11-02T01:30:00-05:00" {
		t.Fatalf("repeated fold occurrence: %v %v", next, err)
	}
	first, weekly, err := ResolveSchedule("every Sunday at 9", time.Date(2026, 3, 1, 8, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	next, err = weekly.next(first, first)
	if err != nil || next.Format(time.RFC3339) != "2026-03-08T09:00:00-04:00" || next.Sub(first) != 167*time.Hour {
		t.Fatalf("weekly used fixed hours: %v %v", next, err)
	}
	lordHowe := zone(t, "Australia/Lord_Howe")
	fold, _, err = ResolveSchedule("tomorrow at 1:45", time.Date(2026, 4, 4, 12, 0, 0, 0, lordHowe))
	if err != nil || fold.Format(time.RFC3339) != "2026-04-05T01:45:00+11:00" {
		t.Fatalf("half-hour fold: %v %v", fold, err)
	}
}

func TestSnoozeVocabulary(t *testing.T) {
	for _, input := range []string{"snooze for ten minutes", "snooze it for ten minutes", "snooze that reminder for ten minutes", "snooze the reminder for ten minutes", "snooze reminder for ten minutes", "snooze the last reminder for ten minutes"} {
		c := Parse(input)
		if c.Kind != Snooze || !c.Reminder || c.Duration != 10*time.Minute || c.Text != "" || c.Error != "" {
			t.Errorf("%s = %+v", input, c)
		}
	}
	c := Parse("please snooze the reminder to Call Sam for ten minutes")
	if c.Kind != Snooze || c.Text != "Call Sam" || c.Duration != 10*time.Minute {
		t.Fatalf("named snooze: %+v", c)
	}
	for _, input := range []string{"snooze the timer for ten minutes", "snooze for -5 minutes", "snooze for 1.5 hours", "snooze for 366 days"} {
		c := Parse(input)
		if c.Kind != Snooze || c.Error == "" && c.Duration != 0 {
			t.Errorf("unsafe snooze: %+v", c)
		}
	}
}

func durableScheduler(t *testing.T, st Saver, c *fakeClock) (*Scheduler, []Timer) {
	t.Helper()
	s, missed, err := New(st, faults.Discard, WithClock(c.Now, c.After))
	if err != nil {
		t.Fatal(err)
	}
	return s, missed
}

func takeDue(t *testing.T, s *Scheduler) Timer {
	t.Helper()
	s.emitDue()
	select {
	case due := <-s.Fired():
		return due
	default:
		t.Fatal("no due reminder")
		return Timer{}
	}
}

func TestRecurringSnoozeSurvivesRestartWithoutChangingPhase(t *testing.T) {
	c := newClock()
	st := store(t, filepath.Join(t.TempDir(), "reminders.db"))
	s, _ := durableScheduler(t, st, c)
	original, err := s.ScheduleReminder(Parse("remind me every ten minutes to Call Sam"))
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(10 * time.Minute)
	if got := takeDue(t, s); got.ID != original.ID {
		t.Fatalf("wrong due: %+v", got)
	}
	s, missed := durableScheduler(t, st, c)
	if len(missed) != 0 {
		t.Fatal("duplicate after restart")
	}
	if _, err := s.Snooze("", 25*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snooze("Call Sam", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	s, _ = durableScheduler(t, st, c)
	live := s.List()
	if len(live) != 1 || !live[0].Deadline.Equal(original.Deadline.Add(15*time.Minute)) || !live[0].Resume.Equal(original.Deadline.Add(10*time.Minute)) {
		t.Fatalf("snooze duplicated/lost phase: %+v", live)
	}
	c.Advance(15 * time.Minute)
	takeDue(t, s)
	live = s.List()
	if len(live) != 1 || !live[0].Deadline.Equal(original.Deadline.Add(20*time.Minute)) || !live[0].Resume.IsZero() {
		t.Fatalf("phase shifted: %+v", live)
	}
	c.Advance(5 * time.Minute)
	takeDue(t, s)
	if _, err := s.Cancel(func(t Timer) bool { return t.ID == original.ID }); err != nil {
		t.Fatal(err)
	}
	s, _ = durableScheduler(t, st, c)
	if _, err := s.Snooze("", time.Minute); err == nil {
		t.Fatal("cancelled series resurrected")
	}
}

func TestRecurringRestartCoalescesDowntimeAndCalendarZone(t *testing.T) {
	loc := zone(t, "America/New_York")
	c := newClock()
	c.now = time.Date(2026, 3, 1, 8, 0, 0, 0, loc)
	st := store(t, filepath.Join(t.TempDir(), "reminders.db"))
	s, _ := durableScheduler(t, st, c)
	original, err := s.ScheduleReminder(Parse("remind me every Sunday at 9 to Call Sam"))
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(30 * 24 * time.Hour)
	// A changed host location must not change the series' original IANA zone.
	c.now = c.now.In(time.UTC)
	s, missed := durableScheduler(t, st, c)
	if len(missed) != 1 || !missed[0].Deadline.Equal(original.Deadline) {
		t.Fatalf("catch-up flood/lost deadline: %+v", missed)
	}
	live := s.List()
	if len(live) != 1 || live[0].Deadline.In(loc).Format(time.RFC3339) != "2026-04-05T09:00:00-04:00" {
		t.Fatalf("calendar restart = %+v", live)
	}
	s, missed = durableScheduler(t, st, c)
	if len(missed) != 0 {
		t.Fatalf("duplicate restart: %+v", missed)
	}
	if _, err := s.Snooze("", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	takeDue(t, s)
	if !s.List()[0].Deadline.Equal(live[0].Deadline) {
		t.Fatal("short snooze moved regular recurrence")
	}
}

func TestOneShotSnoozeHistoryIsDurableBoundedAndIsolated(t *testing.T) {
	c := newClock()
	st := store(t, filepath.Join(t.TempDir(), "reminders.db"))
	s, _ := durableScheduler(t, st, c)
	for i := range 35 {
		if _, err := s.AddReminder(time.Second, fmt.Sprintf("message %d", i)); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Second)
		takeDue(t, s)
	}
	if len(s.List()) != 0 || len(s.Reminders()) != 32 {
		t.Fatalf("history grew or leaked into pending: %d %d", len(s.List()), len(s.Reminders()))
	}
	if _, err := s.Add(time.Second, "one second"); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Second)
	takeDue(t, s)
	s, _ = durableScheduler(t, st, c)
	got, err := s.Snooze("", time.Minute)
	if err != nil || got.Reminder != "message 34" {
		t.Fatalf("timer displaced last reminder: %+v %v", got, err)
	}
	if _, err := s.Snooze("message 0", time.Minute); err == nil {
		t.Fatal("pruned history resurrected")
	}
	s, _ = durableScheduler(t, st, c)
	if len(s.List()) != 1 {
		t.Fatal("snooze not durable")
	}
	if _, err := s.Cancel(func(t Timer) bool { return t.Reminder != "" }); err != nil {
		t.Fatal(err)
	}
	s, _ = durableScheduler(t, st, c)
	if _, err := s.Snooze("", time.Minute); err == nil {
		t.Fatal("cancel all left snoozable history")
	}
}

func TestNamedSnoozeAmbiguityAndLastDueOrdering(t *testing.T) {
	c := newClock()
	s, _ := durableScheduler(t, &flakyStore{}, c)
	later, _ := s.AddReminder(2*time.Minute, "same text")
	if _, err := s.AddReminder(time.Minute, "same text"); err != nil {
		t.Fatal(err)
	}
	c.Advance(3 * time.Minute)
	s.emitDue()
	<-s.Fired()
	<-s.Fired()
	if _, err := s.Snooze("same text", time.Minute); err == nil {
		t.Fatal("ambiguous name selected silently")
	}
	got, err := s.Snooze("", time.Minute)
	if err != nil || got.ID != later.ID {
		t.Fatalf("last due picked last inserted: %+v %v", got, err)
	}
}

func TestReminderMutationsRollbackOnSaveFailure(t *testing.T) {
	c := newClock()
	st := &flakyStore{}
	s, _ := durableScheduler(t, st, c)
	original, err := s.ScheduleReminder(Parse("remind me every minute to call Sam"))
	if err != nil {
		t.Fatal(err)
	}
	st.failFrom(errors.New("disk full"))
	if _, err := s.AddReminder(time.Minute, "second"); err == nil || len(s.List()) != 1 {
		t.Fatal("failed add mutated live state")
	}
	if removed, err := s.Cancel(func(Timer) bool { return true }); err == nil || len(removed) != 0 || s.List()[0] != original {
		t.Fatal("failed cancel mutated live state")
	}
	c.Advance(time.Minute)
	s.emitDue()
	if s.List()[0] != original {
		t.Fatal("failed delivery advanced recurrence")
	}
	st.failFrom(nil)
	takeDue(t, s)
	before := s.List()[0]
	st.failFrom(errors.New("disk full"))
	if _, err := s.Snooze("", 10*time.Minute); err == nil || s.List()[0] != before {
		t.Fatal("failed snooze changed recurrence")
	}
	st.failFrom(nil)
	s, missed := durableScheduler(t, st, c)
	if len(missed) != 0 || s.List()[0] != before {
		t.Fatal("failed mutation persisted")
	}
}

func TestReminderSchemaMigrationPreservesOldRowsAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	deadline := newClock().Now().Add(time.Hour).Format(time.RFC3339Nano)
	_, err = db.Exec(`CREATE TABLE timers (id INTEGER PRIMARY KEY, label TEXT NOT NULL, duration INTEGER NOT NULL, deadline TEXT NOT NULL, reminder TEXT NOT NULL DEFAULT ''); INSERT INTO timers VALUES (1, 'hour', ?, ?, ''), (2, '', ?, ?, 'Call Sam')`, int64(time.Hour), deadline, int64(time.Hour), deadline)
	if err != nil {
		t.Fatal(err)
	}
	st := store(t, path)
	rows, err := st.Load()
	if err != nil || len(rows) != 2 || rows[0].Label != "hour" || rows[1].Reminder != "Call Sam" {
		t.Fatalf("migration lost rows: %+v %v", rows, err)
	}
	// SQLite column names are case-insensitive. An existing STATE column
	// deliberately makes the second ALTER fail after the first succeeded.
	badPath := filepath.Join(t.TempDir(), "rollback.db")
	bad, err := sql.Open("sqlite", "file:"+badPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bad.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := bad.Exec(`CREATE TABLE timers (id INTEGER PRIMARY KEY, label TEXT NOT NULL, duration INTEGER NOT NULL, deadline TEXT NOT NULL, STATE TEXT); INSERT INTO timers VALUES (3, 'keep', 1, ?, '{}')`, deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badPath); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	var reminders, count int
	if err := bad.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('timers') WHERE name='reminder'`).Scan(&reminders); err != nil {
		t.Fatal(err)
	}
	if err := bad.QueryRow(`SELECT COUNT(*) FROM timers WHERE label='keep'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if reminders != 0 || count != 1 {
		t.Fatalf("migration was not atomic: reminder columns=%d rows=%d", reminders, count)
	}
}

func TestStoreFailedReplacementKeepsRecurringAndDeliveredState(t *testing.T) {
	st := store(t, filepath.Join(t.TempDir(), "state.db"))
	c := newClock()
	s, _ := durableScheduler(t, st, c)
	if _, err := s.ScheduleReminder(Parse("remind me every minute to call Sam")); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	takeDue(t, s)
	before, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(append(before, before[0])); err == nil {
		t.Fatal("duplicate IDs should fail the transaction")
	}
	after, err := st.Load()
	if err != nil || len(after) != 1 || after[0] != before[0] {
		t.Fatalf("rollback lost recurring state: %+v %v", after, err)
	}
	if _, err := st.db.Exec(`UPDATE timers SET state='{"repeat":{"kind":"weekly","zone":"Missing/Zone"}}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); err == nil {
		t.Fatal("corrupt recurrence accepted")
	}
}

func TestFailedRecurringStartupTransitionRetriesWithoutFlood(t *testing.T) {
	c := newClock()
	st := &flakyStore{}
	s, _ := durableScheduler(t, st, c)
	first, err := s.ScheduleReminder(Parse("remind me every minute to call Sam"))
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(24 * time.Hour)
	st.failFrom(errors.New("read-only database"))
	restart, missed, err := New(st, faults.Discard, WithClock(c.Now, c.After))
	if err == nil || len(missed) != 0 || len(restart.List()) != 1 || restart.List()[0] != first {
		t.Fatalf("failed startup consumed occurrence: %+v %+v %v", restart.List(), missed, err)
	}
	st.failFrom(nil)
	takeDue(t, restart)
	restart, missed = durableScheduler(t, st, c)
	if len(missed) != 0 || len(restart.List()) != 1 || !restart.List()[0].Deadline.Equal(c.Now().Add(time.Minute)) {
		t.Fatalf("recurring retry repeated or shifted: %+v %+v", restart.List(), missed)
	}
}

func TestOneShotSnoozeCanBeRepeatedAfterDeliveryAndRestart(t *testing.T) {
	c := newClock()
	st := store(t, filepath.Join(t.TempDir(), "repeat-snooze.db"))
	s, _ := durableScheduler(t, st, c)
	first, err := s.AddReminder(time.Minute, "Call Sam")
	if err != nil {
		t.Fatal(err)
	}
	var missed []Timer
	for range 3 {
		c.Advance(time.Minute)
		if due := takeDue(t, s); due.ID != first.ID {
			t.Fatal("snooze created a new reminder")
		}
		s, missed = durableScheduler(t, st, c)
		if len(missed) != 0 || len(s.List()) != 0 {
			t.Fatal("delivered snooze remained pending")
		}
		got, err := s.Snooze("call sam", time.Minute)
		if err != nil || got.ID != first.ID || len(s.List()) != 1 || len(s.Reminders()) != 1 {
			t.Fatalf("snooze duplicated record: %+v %v", got, err)
		}
	}
}

func TestHistoryRetentionKeepsLastOfSimultaneousDeliveries(t *testing.T) {
	c := newClock()
	st := &flakyStore{}
	s, _ := durableScheduler(t, st, c)
	var last Timer
	for i := range 35 {
		var err error
		last, err = s.AddReminder(time.Minute, fmt.Sprintf("simultaneous %d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	c.Advance(time.Minute)
	s, missed := durableScheduler(t, st, c)
	if len(missed) != 35 || len(s.Reminders()) != 32 {
		t.Fatalf("bad bounded history: %d %d", len(missed), len(s.Reminders()))
	}
	got, err := s.Snooze("", time.Minute)
	if err != nil || got.ID != last.ID {
		t.Fatalf("last announcement pruned: %+v %v", got, err)
	}
}
