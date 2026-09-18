package session

import (
	"context"
	"errors"
	stdlog "log"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/vad"
)

func TestRandomTranscriptsNeverReachPlanner(t *testing.T) {
	planner := &countingPlanner{}
	a := &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: 3},
		STT:         &queuedTranscriber{texts: []string{"the television is loud", "someone said hey voice turn on the light", "hey voise turn on the light"}},
		TTS:         testSynthesizer{},
		Brain:       planner,
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
		WakeFuzz:    0,
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner called %d times for non-wake transcripts", planner.calls)
	}
}

func TestLocalControlStopsWithoutPlanner(t *testing.T) {
	reply, stop, handled := localControl("turn yourself off")
	if !handled || !stop || reply != "Turning off." {
		t.Fatalf("got reply=%q stop=%v handled=%v", reply, stop, handled)
	}
}

func TestLocalControlDoesNotCaptureDeviceCommand(t *testing.T) {
	_, _, handled := localControl("turn off the tv")
	if handled {
		t.Fatal("device command must reach the planner")
	}
}

func TestLocalControlReportsAlreadyOnWithoutStopping(t *testing.T) {
	reply, stop, handled := localControl("turn on")
	if !handled || stop || reply != "I'm already on." {
		t.Fatalf("got reply=%q stop=%v handled=%v", reply, stop, handled)
	}
}

type testRecorder struct{}

func (testRecorder) Stream(context.Context) (<-chan []int16, <-chan error) {
	pcm := make(chan []int16)
	errs := make(chan error)
	close(pcm)
	close(errs)
	return pcm, errs
}
func (testRecorder) Format() audio.Format { return audio.Default() }
func (testRecorder) Describe() string     { return "test recorder" }

type testPlayer struct{}

func (testPlayer) PlayWAV(context.Context, []byte) error                { return nil }
func (testPlayer) PlayPCM(context.Context, []int16, audio.Format) error { return nil }
func (testPlayer) Stop() error                                          { return nil }
func (testPlayer) Describe() string                                     { return "test player" }

type testSegmenter struct{ count int }

func (s testSegmenter) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	out := make(chan vad.Utterance, s.count)
	for range s.count {
		out <- vad.Utterance{Format: audio.Default()}
	}
	close(out)
	return out
}

type queuedTranscriber struct {
	texts []string
	next  int
}

func (t *queuedTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	text := t.texts[t.next]
	t.next++
	return text, nil
}
func (*queuedTranscriber) Name() string              { return "test transcriber" }
func (*queuedTranscriber) Available() (bool, string) { return true, "available" }

type testSynthesizer struct{}

func (testSynthesizer) Synthesize(context.Context, string) ([]byte, error) { return nil, nil }
func (testSynthesizer) Name() string                                       { return "test synthesizer" }
func (testSynthesizer) Available() (bool, string)                          { return true, "available" }

type countingPlanner struct{ calls int }

func (p *countingPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	p.calls++
	return brain.Plan{}, nil
}
func (*countingPlanner) Name() string              { return "test planner" }
func (*countingPlanner) Available() (bool, string) { return true, "available" }

type recordingIndicator struct{ states []feedback.State }

func (r *recordingIndicator) Set(s feedback.State) error {
	r.states = append(r.states, s)
	return nil
}
func (*recordingIndicator) Describe() string { return "recording indicator" }

type countingPlayer struct {
	testPlayer
	pcm  int
	wavs int
}

func (p *countingPlayer) PlayPCM(context.Context, []int16, audio.Format) error {
	p.pcm++
	return nil
}
func (p *countingPlayer) PlayWAV(context.Context, []byte) error {
	p.wavs++
	return nil
}

type failingPlanner struct{}

func (failingPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	return brain.Plan{}, errors.New("brain unavailable")
}
func (failingPlanner) Name() string              { return "failing planner" }
func (failingPlanner) Available() (bool, string) { return false, "unavailable" }

func feedbackAssistant(t *testing.T, planner brain.Planner, texts ...string) (*Assistant, *recordingIndicator, *countingPlayer) {
	t.Helper()
	ind := &recordingIndicator{}
	player := &countingPlayer{}
	a := &Assistant{
		Recorder: testRecorder{},
		Player:   player,
		VAD:      testSegmenter{count: len(texts)},
		STT:      &queuedTranscriber{texts: texts},
		TTS:      testSynthesizer{},
		Brain:    planner,
		Devices:  device.NewRegistry(),
		Feedback: &feedback.Notifier{
			Indicator: ind,
			Player:    player,
			Sound:     []int16{1, 2, 3},
			Format:    audio.Default(),
		},
		WakePhrases: []string{"hey voice"},
		WakeFuzz:    0,
	}
	return a, ind, player
}

func TestEachAcceptedWakeLightsUpThenReturnsToIdle(t *testing.T) {
	a, ind, player := feedbackAssistant(t, &countingPlanner{},
		"hey voice turn on the light",
		"hey voice turn off the light",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if player.pcm != 2 {
		t.Errorf("acknowledgement sound played %d times for 2 accepted wakes, want 2", player.pcm)
	}
	want := []feedback.State{feedback.Idle, feedback.Listening, feedback.Idle, feedback.Listening, feedback.Idle}
	if got := transitions(ind.states); !equal(got, want) {
		t.Errorf("indicator transitions = %v, want %v", got, want)
	}
}

func TestNonWakeSpeechProducesNoLightOrSound(t *testing.T) {
	a, ind, player := feedbackAssistant(t, &countingPlanner{},
		"the television is loud",
		"someone said hey voice turn on the light",
		"hey voise turn on the light",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if player.pcm != 0 {
		t.Errorf("played %d acknowledgement sounds for non-wake speech, want 0", player.pcm)
	}
	if hasState(ind.states, feedback.Listening) {
		t.Errorf("indicator lit for non-wake speech: %v", ind.states)
	}
}

func TestIndicatorReturnsToIdleWhenCommandFails(t *testing.T) {
	a, ind, player := feedbackAssistant(t, failingPlanner{},
		"hey voice turn on the light",
		"hey voice turn on the light",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if player.pcm != 2 {
		t.Errorf("acknowledgement sound played %d times, want 2", player.pcm)
	}
	want := []feedback.State{feedback.Idle, feedback.Listening, feedback.Idle, feedback.Listening, feedback.Idle}
	if got := transitions(ind.states); !equal(got, want) {
		t.Errorf("indicator transitions = %v, want %v", got, want)
	}
}

func TestSpokenTurnOffLeavesIndicatorIdle(t *testing.T) {
	a, ind, _ := feedbackAssistant(t, &countingPlanner{}, "hey voice turn yourself off")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := ind.states[len(ind.states)-1]; last != feedback.Idle {
		t.Errorf("indicator left in %v after turning off, want idle", last)
	}
}

func TestAssistantWithoutFeedbackStillHandlesCommands(t *testing.T) {
	planner := &countingPlanner{}
	a := &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: 1},
		STT:         &queuedTranscriber{texts: []string{"hey voice turn on the light"}},
		TTS:         testSynthesizer{},
		Brain:       planner,
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 {
		t.Fatalf("planner called %d times without a feedback notifier, want 1", planner.calls)
	}
}

// transitions collapses repeated writes so a test pins the state changes a
// user would see, not how many times the same state was written.
func transitions(states []feedback.State) []feedback.State {
	out := make([]feedback.State, 0, len(states))
	for i, s := range states {
		if i == 0 || states[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func equal(a, b []feedback.State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasState(states []feedback.State, want feedback.State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// closingRecorder reports whether its cleanup ran before Run returned, which
// is what decides whether a shutdown leaves the speakerphone off hook.
type closingRecorder struct {
	cleaned chan struct{}
	slow    time.Duration
}

func (r *closingRecorder) Stream(ctx context.Context) (<-chan []int16, <-chan error) {
	pcm := make(chan []int16)
	errs := make(chan error)
	go func() {
		defer close(pcm)
		defer close(errs)
		<-ctx.Done()
		time.Sleep(r.slow) // stands in for clearing the off-hook report
		close(r.cleaned)
	}()
	return pcm, errs
}
func (*closingRecorder) Format() audio.Format { return audio.Default() }
func (*closingRecorder) Describe() string     { return "closing recorder" }

func TestShutdownWaitsForCaptureCleanup(t *testing.T) {
	rec := &closingRecorder{cleaned: make(chan struct{}), slow: 50 * time.Millisecond}
	a := &Assistant{
		Recorder:    rec,
		Player:      testPlayer{},
		VAD:         idleSegmenter{},
		STT:         &queuedTranscriber{},
		TTS:         testSynthesizer{},
		Brain:       &countingPlanner{},
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-rec.cleaned:
	default:
		t.Fatal("Run returned before capture finished its cleanup")
	}
}

// A capture command that never stops must not keep the service from exiting.
func TestShutdownGivesUpOnAStuckCapture(t *testing.T) {
	rec := &closingRecorder{cleaned: make(chan struct{}), slow: 10 * time.Second}
	a := &Assistant{
		Recorder:    rec,
		Player:      testPlayer{},
		VAD:         idleSegmenter{},
		STT:         &queuedTranscriber{},
		TTS:         testSynthesizer{},
		Brain:       &countingPlanner{},
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 2*captureStopTimeout {
		t.Errorf("shutdown took %v with a stuck capture, want about %v", waited, captureStopTimeout)
	}
}

// A bare wake phrase is accepted: it acknowledges locally and asks for the
// request, without reaching the planner. The README says so, and nothing
// asserted it until now.
func TestBareWakePhraseAsksAndSkipsThePlanner(t *testing.T) {
	a, ind, player := feedbackAssistant(t, &countingPlanner{}, "hey voice")
	planner := a.Brain.(*countingPlanner)
	tts := &recordingSynthesizer{}
	a.TTS = tts
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 {
		t.Errorf("planner called %d times for a bare wake phrase", planner.calls)
	}
	if !saidContaining(tts.spoken(), "followed by your request") {
		t.Errorf("did not ask for the request: %q", tts.spoken())
	}
	if player.pcm != 1 {
		t.Errorf("acknowledgement sound played %d times, want 1", player.pcm)
	}
	want := []feedback.State{feedback.Idle, feedback.Listening, feedback.Idle}
	if got := transitions(ind.states); !equal(got, want) {
		t.Errorf("indicator transitions = %v, want %v", got, want)
	}
}

// A silent device and a broken transcriber used to produce the same journal:
// one line after a successful transcription, nothing before it. Each stage
// must now report its own outcome, so a future silence names the stage that
// stopped instead of leaving five rounds of guessing.
func TestEachPipelineStageIsObservable(t *testing.T) {
	var log strings.Builder
	a, _, _ := feedbackAssistant(t, &countingPlanner{}, "hey voice turn on the light", "the television is loud")
	a.Logger = stdlog.New(&log, "", 0)
	a.STT = &queuedTranscriber{texts: []string{"hey voice turn on the light", "the television is loud"}}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"stage=capture state=starting",
		"stage=segment state=ok",
		"stage=transcribe state=ok",
		"stage=wake state=matched",
		"stage=wake state=nomatch",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("journal is missing %q:\n%s", want, log.String())
		}
	}
}

// A transcriber that cannot start must say so per utterance, and must not be
// mistaken for a device that heard nothing.
func TestTranscribeFailureIsDistinguishableFromSilence(t *testing.T) {
	var log strings.Builder
	a, _, _ := feedbackAssistant(t, &countingPlanner{}, "ignored")
	a.Logger = stdlog.New(&log, "", 0)
	a.STT = brokenTranscriber{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	if !strings.Contains(out, "stage=segment state=ok") {
		t.Errorf("a segmented utterance was not reported:\n%s", out)
	}
	if !strings.Contains(out, "stage=transcribe state=failed") {
		t.Errorf("the transcriber failure was not reported as its own stage:\n%s", out)
	}
}

type brokenTranscriber struct{}

func (brokenTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	return "", errors.New("whisper-cli: exit status 127: libwhisper.so.1: cannot open shared object file")
}
func (brokenTranscriber) Name() string              { return "broken transcriber" }
func (brokenTranscriber) Available() (bool, string) { return false, "cannot start" }
