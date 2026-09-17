package session

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/device"
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
	return nil, nil
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
	sched, missed, err := timer.New(timer.Store{Path: filepath.Join(t.TempDir(), "timers.json")})
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
