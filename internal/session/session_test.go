package session

import (
	"context"
	"errors"
	stdlog "log"
	"strings"
	"sync"
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

func TestStreamingWakeSegmentReachesPlannerWithoutWakeText(t *testing.T) {
	planner := &countingPlanner{}
	a := &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         wakeMatchedSegmenter{},
		STT:         &queuedTranscriber{texts: []string{"what time is it"}},
		TTS:         testSynthesizer{},
		Brain:       planner,
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 {
		t.Fatalf("planner called %d times, want 1 after local keyword match", planner.calls)
	}
}

func TestAcknowledgedFollowUpDoesNotReplayWakeSound(t *testing.T) {
	planner := &countingPlanner{}
	a, _, player := feedbackAssistant(t, planner)
	a.VAD = wakeMatchedSegmenter{acknowledged: true}
	a.STT = &queuedTranscriber{texts: []string{"what time is it"}}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if player.pcm != 0 {
		t.Fatalf("follow-up replayed acknowledgement sound %d times", player.pcm)
	}
	if planner.calls != 1 {
		t.Fatalf("planner called %d times, want 1", planner.calls)
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

type wakeMatchedSegmenter struct{ acknowledged bool }

func (s wakeMatchedSegmenter) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	out := make(chan vad.Utterance, 1)
	out <- vad.Utterance{
		PCM:              []int16{1, 2, 3},
		Format:           audio.Default(),
		WakeMatched:      true,
		WakeAcknowledged: s.acknowledged,
		Keyword:          "HEY_VOICE",
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

func (testSynthesizer) Synthesize(context.Context, string) ([]byte, error) {
	return audio.EncodeWAV([]int16{1}, audio.Default()), nil
}
func (testSynthesizer) Name() string              { return "test synthesizer" }
func (testSynthesizer) Available() (bool, string) { return true, "available" }

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
	// Durations are part of the contract: a stage line without a time cannot
	// answer where an interaction went.
	for _, want := range []string{"stage=plan state=ok ms=", "stage=speak state=ok ms="} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("journal is missing %q, so the stage cannot be timed:\n%s", want, log.String())
		}
	}
	for _, want := range []string{
		"stage=capture state=starting",
		"stage=segment state=ok",
		"stage=transcribe state=ok",
		"stage=wake state=matched",
		"stage=wake state=nomatch",
		// The planner and the speaker: unlogged until a real interaction took
		// about a minute and the instrumented stages accounted for six
		// seconds of it. A pipeline where the slowest stage is invisible
		// cannot be diagnosed, only guessed at.
		"stage=plan state=ok",
		"stage=speak state=ok",
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

// say() was a second entry point to Speak, used by every timer prompt,
// confirmation and announcement plus the planner-failure fallback - eight
// production callsites that stayed invisible while this PR claimed every
// callsite was timed. The claim is only true if the say path logs too.
func TestTimerSpeechIsTimedLikeEveryOtherReply(t *testing.T) {
	var log strings.Builder
	a, _, _ := feedbackAssistant(t, &countingPlanner{}, "hey voice")
	a.Logger = stdlog.New(&log, "", 0)
	a.say(context.Background(), "your timer has finished")
	if got := log.String(); !strings.Contains(got, "stage=speak state=ok ms=") {
		t.Errorf("say() did not emit a timed speak stage:\n%s", got)
	}
}

// A sub-millisecond stage must not log ms=0, which reads as "not measured" -
// the very ambiguity this instrumentation exists to remove.
func TestStageDurationsKeepSubMillisecondResolution(t *testing.T) {
	var log strings.Builder
	a, _, _ := feedbackAssistant(t, &countingPlanner{}, "hey voice")
	a.Logger = stdlog.New(&log, "", 0)
	a.say(context.Background(), "x")
	line := log.String()
	if !strings.Contains(line, "ms=") {
		t.Fatalf("no duration logged:\n%s", line)
	}
	// The fakes are instant, so this is precisely the case that used to log
	// ms=0. A fractional value proves the resolution survived.
	if strings.Contains(line, "ms=0 ") || strings.HasSuffix(strings.TrimSpace(line), "ms=0") {
		t.Errorf("duration truncated to an integer zero, indistinguishable from unmeasured:\n%s", line)
	}
}

// A remote primary is safe only when a local transcript has already matched
// the wake phrase and found a command. This test asserts the privacy boundary,
// not just the final command: ambient speech and a bare wake phrase must
// produce zero remote calls.
func TestLocalWakeGatePreventsRemoteTranscriptionOfAmbientSpeech(t *testing.T) {
	planner := &countingPlanner{}
	gate := &countingTranscriber{texts: []string{"people talking nearby", "hey voice", "hey voice pause the music"}}
	remote := &countingTranscriber{texts: []string{"Hey Voice, pause the music."}}
	a := &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: 3},
		WakeSTT:     gate,
		STT:         remote,
		TTS:         testSynthesizer{},
		Brain:       planner,
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gate.calls != 3 {
		t.Fatalf("local gate calls = %d, want one per utterance", gate.calls)
	}
	if remote.calls != 1 {
		t.Fatalf("remote transcription calls = %d, want only the accepted wake utterance", remote.calls)
	}
	if planner.calls != 1 {
		t.Fatalf("planner calls = %d, want one accepted command", planner.calls)
	}
}

type countingTranscriber struct {
	texts []string
	calls int
}

func (t *countingTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	text := t.texts[t.calls]
	t.calls++
	return text, nil
}
func (*countingTranscriber) Name() string              { return "counting transcriber" }
func (*countingTranscriber) Available() (bool, string) { return true, "available" }

type heldTranscriber struct {
	entered chan struct{}
	release chan struct{}
}

func (t *heldTranscriber) Transcribe(ctx context.Context, _ []int16, _ audio.Format) (string, error) {
	close(t.entered)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.release:
		return "what time is it", nil
	}
}
func (*heldTranscriber) Name() string              { return "held transcriber" }
func (*heldTranscriber) Available() (bool, string) { return true, "available" }

type replyPlanner struct{}

func (replyPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	return brain.Plan{Speak: "It is noon."}, nil
}
func (replyPlanner) Name() string              { return "reply planner" }
func (replyPlanner) Available() (bool, string) { return true, "available" }

// lifecyclePlayer holds processing playback until its context is cancelled,
// exactly as the real command player does. It makes overlap with response
// speech observable instead of relying on scheduler timing.
type lifecyclePlayer struct {
	started chan struct{}
	stopped chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	thinking  bool
	overlap   bool
}

func newLifecyclePlayer() *lifecyclePlayer {
	return &lifecyclePlayer{started: make(chan struct{}), stopped: make(chan struct{})}
}

func (p *lifecyclePlayer) PlayPCM(ctx context.Context, _ []int16, _ audio.Format) error {
	p.mu.Lock()
	p.thinking = true
	p.mu.Unlock()
	p.startOnce.Do(func() { close(p.started) })
	<-ctx.Done()
	p.mu.Lock()
	p.thinking = false
	p.mu.Unlock()
	p.stopOnce.Do(func() { close(p.stopped) })
	return ctx.Err()
}

func (p *lifecyclePlayer) PlayWAV(context.Context, []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.overlap = p.overlap || p.thinking
	return nil
}
func (*lifecyclePlayer) Stop() error      { return nil }
func (*lifecyclePlayer) Describe() string { return "lifecycle player" }

func (p *lifecyclePlayer) overlapped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overlap
}

func TestStreamingWakeThinksDuringTranscriptionAndStopsBeforeSpeech(t *testing.T) {
	var logs strings.Builder
	player := newLifecyclePlayer()
	transcriber := &heldTranscriber{entered: make(chan struct{}), release: make(chan struct{})}
	a := &Assistant{
		Recorder: testRecorder{},
		Player:   player,
		VAD:      wakeMatchedSegmenter{acknowledged: true},
		STT:      transcriber,
		TTS:      testSynthesizer{},
		Brain:    replyPlanner{},
		Devices:  device.NewRegistry(),
		Feedback: &feedback.Notifier{
			Player:        player,
			Working:       []int16{1, 2, 3},
			ThinkingDelay: time.Millisecond,
			Format:        audio.Default(),
			Logger:        stdlog.New(&logs, "", 0),
		},
		WakePhrases: []string{"hey voice"},
		Logger:      stdlog.New(&logs, "", 0),
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case <-transcriber.entered:
	case <-time.After(time.Second):
		t.Fatal("transcription did not start")
	}
	select {
	case <-player.started:
		// The transcriber is still blocked: feedback therefore covers STT.
	case <-time.After(time.Second):
		t.Fatal("thinking feedback did not start while transcription was in progress")
	}
	close(transcriber.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-player.stopped:
	default:
		t.Fatal("processing playback was not finished when Run returned")
	}
	if player.overlapped() {
		t.Fatal("processing playback overlapped the spoken response")
	}
	for _, want := range []string{
		"stage=thinking state=started delay_ms=",
		"stage=thinking state=stopped ms=",
		"stage=speak state=ok",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("journal is missing %q:\n%s", want, logs.String())
		}
	}
}

func TestTranscriptionFailureStopsProcessingPlayback(t *testing.T) {
	player := newLifecyclePlayer()
	a := &Assistant{
		Recorder: testRecorder{},
		Player:   player,
		VAD:      wakeMatchedSegmenter{acknowledged: true},
		STT: transcriberFunc(func(ctx context.Context, _ []int16, _ audio.Format) (string, error) {
			select {
			case <-player.started:
				return "", errors.New("transcription failed")
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}),
		TTS:     testSynthesizer{},
		Brain:   &countingPlanner{},
		Devices: device.NewRegistry(),
		Feedback: &feedback.Notifier{
			Player:        player,
			Working:       []int16{1},
			ThinkingDelay: time.Millisecond,
			Format:        audio.Default(),
		},
		WakePhrases: []string{"hey voice"},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-player.stopped:
	default:
		t.Fatal("transcription failure left processing playback running")
	}
}

type transcriberFunc func(context.Context, []int16, audio.Format) (string, error)

func (f transcriberFunc) Transcribe(ctx context.Context, pcm []int16, format audio.Format) (string, error) {
	return f(ctx, pcm, format)
}
func (transcriberFunc) Name() string              { return "function transcriber" }
func (transcriberFunc) Available() (bool, string) { return true, "available" }
