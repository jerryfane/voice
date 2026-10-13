package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/timer"
	"github.com/jerryfane/voice/internal/vad"
)

type recordingSynthesizer struct {
	mu   sync.Mutex
	said []string
}

func (r *recordingSynthesizer) Synthesize(_ context.Context, text string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.said = append(r.said, text)
	return audio.EncodeWAV([]int16{1}, audio.Default()), nil
}
func (*recordingSynthesizer) Name() string              { return "recording synthesizer" }
func (*recordingSynthesizer) Available() (bool, string) { return true, "available" }
func (r *recordingSynthesizer) spoken() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.said...)
}

func timerAssistant(t *testing.T, texts ...string) (*Assistant, *recordingSynthesizer, *countingPlanner) {
	t.Helper()
	tts := &recordingSynthesizer{}
	planner := &countingPlanner{}
	store, err := timer.Open(filepath.Join(t.TempDir(), "timers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("closing the timer store: %v", err)
		}
	})
	sched, missed, err := timer.New(store, faults.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: len(texts)},
		STT:         &queuedTranscriber{texts: texts},
		TTS:         tts,
		Brain:       planner,
		Devices:     device.NewRegistry(),
		Timers:      sched,
		Missed:      missed,
		WakePhrases: []string{"hey voice"},
	}, tts, planner
}

// idleSegmenter keeps the utterance channel open, the way a running capture
// does: a scheduler announcement must not depend on someone speaking.
type idleSegmenter struct{}

func (idleSegmenter) Run(ctx context.Context, _ <-chan []int16, _ audio.Format) <-chan vad.Utterance {
	out := make(chan vad.Utterance)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out
}

func saidContaining(said []string, want string) bool {
	for _, s := range said {
		if strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
			return true
		}
	}
	return false
}

// A timer must be settable with no model: the planner is the thing that needs
// the network, and a kitchen timer cannot.
func TestSettingATimerNeverReachesThePlanner(t *testing.T) {
	a, tts, planner := timerAssistant(t, "hey voice set a timer for ten minutes")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 {
		t.Errorf("planner called %d times for a timer request", planner.calls)
	}
	if !saidContaining(tts.spoken(), "10 minutes timer started") {
		t.Errorf("confirmation = %q", tts.spoken())
	}
	if live := a.Timers.List(); len(live) != 1 || live[0].Duration != 10*time.Minute {
		t.Errorf("live timers = %+v", live)
	}
}

func TestTimerQueryAndCancelAnswerLocally(t *testing.T) {
	a, tts, planner := timerAssistant(t,
		"hey voice set a timer for ten minutes",
		"hey voice how long is left on my timer",
		"hey voice cancel the ten minute timer",
		"hey voice how long is left on my timer",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 {
		t.Errorf("planner called %d times", planner.calls)
	}
	said := tts.spoken()
	if !saidContaining(said, "left on the 10 minutes timer") {
		t.Errorf("query answer missing from %q", said)
	}
	if !saidContaining(said, "cancelled") {
		t.Errorf("cancel confirmation missing from %q", said)
	}
	if !saidContaining(said, "no timers running") {
		t.Errorf("post-cancel query answer missing from %q", said)
	}
	if live := a.Timers.List(); len(live) != 0 {
		t.Errorf("timers still live after cancel: %+v", live)
	}
}

// Expiry has to be announced without anyone saying the wake phrase.
func TestExpiredTimerIsAnnouncedWithoutAWakePhrase(t *testing.T) {
	a, tts, _ := timerAssistant(t)
	a.VAD = idleSegmenter{}
	if _, err := a.Timers.Add(20*time.Millisecond, "20 milliseconds"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !saidContaining(tts.spoken(), "timer is done") {
		if time.Now().After(deadline) {
			t.Fatalf("expiry never announced; said %q", tts.spoken())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A timer that came due while Voice was down is reported as expired, with how
// long ago, rather than announced as if it had just finished.
func TestMissedTimersAreReportedAtStartup(t *testing.T) {
	a, tts, _ := timerAssistant(t)
	a.Missed = []timer.Timer{{
		ID: 1, Label: "five minute", Duration: 5 * time.Minute,
		Deadline: time.Now().Add(-3 * time.Minute),
	}}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	said := tts.spoken()
	if !saidContaining(said, "5 minutes timer expired") || !saidContaining(said, "while i was off") {
		t.Errorf("missed-timer report = %q", said)
	}
}

// Timer speech must not bypass the wake gate.
func TestTimerVocabularyWithoutWakePhraseIsIgnored(t *testing.T) {
	a, tts, _ := timerAssistant(t, "set a timer for ten minutes")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if live := a.Timers.List(); len(live) != 0 {
		t.Errorf("unwaked speech set a timer: %+v", live)
	}
	if len(tts.spoken()) != 0 {
		t.Errorf("unwaked speech produced %q", tts.spoken())
	}
}

// A cancellation that cannot be persisted leaves the live timer untouched.
func TestCancelReportsAPersistenceFailure(t *testing.T) {
	a, tts, _ := timerAssistant(t, "hey voice cancel the ten minute timer")
	sched, _, err := timer.New(failingStore{initial: []timer.Timer{{ID: 1, Duration: 10 * time.Minute, Label: "ten minute", Deadline: time.Now().Add(10 * time.Minute)}}}, faults.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a.Timers = sched
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !saidContaining(tts.spoken(), "could not save") {
		t.Errorf("cancel said %q, expected the persistence failure to be reported", tts.spoken())
	}
	if len(sched.List()) != 1 {
		t.Fatal("failed cancellation changed live timers")
	}
}

// failingStore accepts loads and rejects every save.
type failingStore struct{ initial []timer.Timer }

func (f failingStore) Load() ([]timer.Timer, error) {
	return append([]timer.Timer(nil), f.initial...), nil
}
func (failingStore) Save([]timer.Timer) error { return errStore }

var errStore = errors.New("state directory is read-only")

func TestRemindersSetListAndCancelWithoutPlanner(t *testing.T) {
	a, tts, planner := timerAssistant(t,
		"hey voice remind me in ten minutes to Call Sam",
		"hey voice what reminders do I have",
		"hey voice cancel the reminder to call Sam",
		"hey voice list my reminders",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 || len(a.Timers.List()) != 0 {
		t.Fatalf("local reminders leaked: planner=%d pending=%+v", planner.calls, a.Timers.List())
	}
	for _, want := range []string{"I'll remind you in 10 minutes to Call Sam", "In 10 minutes: Call Sam", "Cancelled your reminder to Call Sam", "no pending reminders"} {
		if !saidContaining(tts.spoken(), want) {
			t.Errorf("missing %q in %q", want, tts.spoken())
		}
	}
}

func TestTimerAndReminderCancellationStaySeparate(t *testing.T) {
	a, _, _ := timerAssistant(t)
	if _, err := a.Timers.Add(time.Hour, "one hour"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Timers.AddReminder(time.Hour, "call Sam"); err != nil {
		t.Fatal(err)
	}
	a.cancelTimers(timer.Command{Kind: timer.Cancel, All: true})
	if live := a.Timers.List(); len(live) != 1 || live[0].Reminder != "call Sam" {
		t.Fatalf("cancel all timers touched reminder: %+v", live)
	}
	if _, err := a.Timers.Add(time.Hour, "one hour"); err != nil {
		t.Fatal(err)
	}
	a.cancelReminders(timer.Command{Kind: timer.Cancel, Reminder: true, All: true})
	if live := a.Timers.List(); len(live) != 1 || live[0].Reminder != "" {
		t.Fatalf("cancel all reminders touched timer: %+v", live)
	}
}

func TestAmbiguousReminderCancelDoesNotGuess(t *testing.T) {
	a, _, _ := timerAssistant(t)
	for _, text := range []string{"call Sam", "call Alex"} {
		if _, err := a.Timers.AddReminder(time.Hour, text); err != nil {
			t.Fatal(err)
		}
	}
	reply := a.cancelReminders(timer.Command{Kind: timer.Cancel, Reminder: true})
	if len(a.Timers.List()) != 2 || !strings.Contains(reply, "more than one") {
		t.Fatalf("ambiguous cancel = %q, %+v", reply, a.Timers.List())
	}
	reply = a.cancelReminders(timer.Command{Kind: timer.Cancel, Reminder: true, Text: "call Sam"})
	if live := a.Timers.List(); len(live) != 1 || live[0].Reminder != "call Alex" || !strings.Contains(reply, "call Sam") {
		t.Fatalf("targeted cancel = %q, %+v", reply, live)
	}
}

func TestReminderNotificationsSpeakContentAndHonestOverdueAge(t *testing.T) {
	a, tts, planner := timerAssistant(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	a.announceFired(context.Background(), timer.Timer{Reminder: "call Sam", Deadline: now})
	a.announceFired(context.Background(), timer.Timer{Reminder: "check the oven", Deadline: now.Add(-3 * time.Minute)})
	a.announceMissed(context.Background(), []timer.Timer{{Reminder: "take out the bins", Deadline: now.Add(-time.Hour)}})
	for _, want := range []string{"Reminder: call Sam", "due 3 minutes ago: check the oven", "While I was off", "take out the bins", "due 1 hour ago"} {
		if !saidContaining(tts.spoken(), want) {
			t.Errorf("missing %q in %q", want, tts.spoken())
		}
	}
	if planner.calls != 0 {
		t.Fatal("delivery sent reminder contents to planner")
	}
}

func TestReminderFiresWithoutWakePhrase(t *testing.T) {
	a, tts, _ := timerAssistant(t)
	a.VAD = idleSegmenter{}
	if _, err := a.Timers.AddReminder(20*time.Millisecond, "call Sam"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !saidContaining(tts.spoken(), "Reminder: call Sam") {
		if time.Now().After(deadline) {
			t.Fatalf("reminder never announced: %q", tts.spoken())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedReminderAndDisabledSchedulerStayHonest(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		a, tts, planner := timerAssistant(t, "hey voice remind me every Friday to put out the bins")
		if disabled {
			a.Timers = nil
		}
		if err := a.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := "I did not set the reminder"
		if disabled {
			want = "scheduler is disabled"
		}
		if planner.calls != 0 || !saidContaining(tts.spoken(), want) {
			t.Fatalf("unsupported reminder: planner=%d speech=%q", planner.calls, tts.spoken())
		}
	}
}

type failedReminderSynthesizer struct{ recordingSynthesizer }

func (*failedReminderSynthesizer) Synthesize(context.Context, string) ([]byte, error) {
	return nil, errors.New("speaker unavailable")
}

func TestReminderControlReportsFailedSpeechWithoutLosingSchedule(t *testing.T) {
	a, _, _ := timerAssistant(t)
	a.TTS = &failedReminderSynthesizer{}
	handled, spoken := a.timerControl(context.Background(), "remind me in ten minutes to call Sam", func() {})
	if !handled || spoken || len(a.Timers.List()) != 1 {
		t.Fatalf("handled=%v spoken=%v pending=%+v", handled, spoken, a.Timers.List())
	}
	if a.announceFired(context.Background(), timer.Timer{Reminder: "call Sam", Deadline: time.Now()}) {
		t.Fatal("failed reminder announcement reported success")
	}
}

func TestCalendarControlUsesPersistedZoneAndNeverPlanner(t *testing.T) {
	a, tts, planner := timerAssistant(t)
	loc, err := time.LoadLocation("Asia/Dubai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, loc)
	a.Now = func() time.Time { return now }
	st, err := timer.Open(filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	a.Timers, _, err = timer.New(st, faults.Discard, timer.WithClock(a.Now, time.After))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"remind me tomorrow at nine to Call Sam",
		"remind me every Monday at 9 a.m. to water the plants",
		"list reminders",
	} {
		stopped := false
		handled, spoken := a.timerControl(context.Background(), command, func() { stopped = true })
		if !handled || !spoken || !stopped {
			t.Fatalf("local calendar command not handled: %s", command)
		}
	}
	for _, want := range []string{"2026-10-14 at 09:00", "Asia/Dubai", "+04:00", "repeating every Monday", "Call Sam"} {
		if !saidContaining(tts.spoken(), want) {
			t.Errorf("missing %q: %q", want, tts.spoken())
		}
	}
	if planner.calls != 0 || len(a.Timers.List()) != 2 {
		t.Fatalf("planner=%d live=%+v", planner.calls, a.Timers.List())
	}
	for _, command := range []string{
		"remind me today at 9 to call Sam",
		"remind me on 2026-02-30 at 9 to call Sam",
		"remind me every month at 9 to call Sam",
		"remind me tomorrow at 25 to call Sam",
		"remind me not tomorrow at 9 to call Sam",
		"snooze the timer for ten minutes",
	} {
		handled, _ := a.timerControl(context.Background(), command, func() {})
		if !handled || len(a.Timers.List()) != 2 {
			t.Fatalf("unsafe calendar command %q: handled=%v live=%+v", command, handled, a.Timers.List())
		}
	}
	if handled, _ := a.timerControl(context.Background(), "do not remind me tomorrow at nine to call Sam", func() {}); handled || len(a.Timers.List()) != 2 {
		t.Fatal("negated command created a reminder")
	}
}

func TestSpokenSnoozeAfterRestartAndCancelRevokesIt(t *testing.T) {
	a, tts, planner := timerAssistant(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	st, err := timer.Open(filepath.Join(t.TempDir(), "snooze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	delivered := timer.Timer{ID: 7, Reminder: "Call Sam", Duration: time.Minute, Deadline: now.Add(-time.Minute), LastDelivered: now.Add(-time.Minute), Archived: true}
	if err := st.Save([]timer.Timer{delivered}); err != nil {
		t.Fatal(err)
	}
	a.Timers, _, err = timer.New(st, faults.Discard, timer.WithClock(a.Now, time.After))
	if err != nil {
		t.Fatal(err)
	}
	if handled, spoken := a.timerControl(context.Background(), "snooze that reminder for ten minutes", func() {}); !handled || !spoken {
		t.Fatal("snooze not handled")
	}
	if live := a.Timers.List(); len(live) != 1 || live[0].ID != 7 || !live[0].Deadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("snooze = %+v", live)
	}
	a.Timers, _, err = timer.New(st, faults.Discard, timer.WithClock(a.Now, time.After))
	if err != nil {
		t.Fatal(err)
	}
	a.timerControl(context.Background(), "cancel the reminder to call Sam", func() {})
	a.timerControl(context.Background(), "snooze the last reminder for ten minutes", func() {})
	if len(a.Timers.List()) != 0 || planner.calls != 0 {
		t.Fatal("cancelled reminder resurrected or leaked to planner")
	}
	for _, want := range []string{"Snoozed your reminder to Call Sam for 10 minutes", "Cancelled your reminder to Call Sam", "no matching delivered reminder"} {
		if !saidContaining(tts.spoken(), want) {
			t.Errorf("missing %q: %q", want, tts.spoken())
		}
	}
}

func TestSaveFailureDoesNotPromiseReminderOrTimer(t *testing.T) {
	a, tts, _ := timerAssistant(t)
	var err error
	a.Timers, _, err = timer.New(failingStore{}, faults.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"remind me in ten minutes to call Sam", "set a timer for ten minutes"} {
		if handled, _ := a.timerControl(context.Background(), command, func() {}); !handled {
			t.Fatalf("not handled: %s", command)
		}
	}
	if len(a.Timers.List()) != 0 {
		t.Fatal("unsaved requests scheduled")
	}
	for _, text := range tts.spoken() {
		if strings.Contains(text, "I'll remind") || strings.Contains(text, "timer started") {
			t.Fatalf("false promise: %q", text)
		}
	}
}
