package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/vad"
)

type conversationWindowEvent struct {
	until  time.Time
	turns  int
	spoken int32
}

type observedConversationGate struct {
	in      <-chan vad.Utterance
	windows chan conversationWindowEvent
	spoken  *atomic.Int32
}

func (g *observedConversationGate) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	return g.in
}
func (g *observedConversationGate) SetInFlightWindow(until time.Time, turns int) {
	g.windows <- conversationWindowEvent{until: until, turns: turns, spoken: g.spoken.Load()}
}
func (*observedConversationGate) Suspend()                   {}
func (*observedConversationGate) ResumeAfter(time.Duration)  {}
func (*observedConversationGate) Accepts(vad.Utterance) bool { return true }

func (g *observedConversationGate) awaitWindow(t *testing.T, spoken int32, open bool) conversationWindowEvent {
	t.Helper()
	for {
		event := awaitAsync(t, g.windows)
		if event.spoken >= spoken && (!event.until.IsZero() && event.turns > 0) == open {
			return event
		}
	}
}

type conversationSynthesizer struct {
	calls  atomic.Int32
	spoken chan string
	failAt int32
}

func (s *conversationSynthesizer) Synthesize(_ context.Context, text string) ([]byte, error) {
	n := s.calls.Add(1)
	s.spoken <- text
	if n == s.failAt {
		return nil, errors.New("synthesis unavailable")
	}
	return audio.EncodeWAV([]int16{1}, audio.Default()), nil
}
func (*conversationSynthesizer) Name() string              { return "conversation synthesizer" }
func (*conversationSynthesizer) Available() (bool, string) { return true, "available" }

type conversationPlanner struct{ calls atomic.Int32 }

func (p *conversationPlanner) Plan(_ context.Context, text string, _ []device.Info) (brain.Plan, error) {
	p.calls.Add(1)
	return brain.Plan{Speak: "Answer to " + text}, nil
}
func (*conversationPlanner) Name() string              { return "conversation planner" }
func (*conversationPlanner) Available() (bool, string) { return true, "available" }

func conversationAssistant(texts ...string) (*Assistant, chan vad.Utterance, *observedConversationGate, *conversationSynthesizer, *conversationPlanner) {
	in := make(chan vad.Utterance)
	synth := &conversationSynthesizer{spoken: make(chan string, 16)}
	gate := &observedConversationGate{in: in, windows: make(chan conversationWindowEvent, 128), spoken: &synth.calls}
	planner := &conversationPlanner{}
	return &Assistant{
		Recorder: testRecorder{}, Player: testPlayer{}, VAD: gate,
		STT: &queuedTranscriber{texts: texts}, TTS: synth, Brain: planner,
		Devices: device.NewRegistry(), WakePhrases: []string{"hey voice"},
		ConversationWindow: time.Minute, ConversationTurns: 2,
	}, in, gate, synth, planner
}

func runConversation(t *testing.T, a *Assistant, in chan vad.Utterance) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	finish := func() {
		t.Helper()
		once.Do(func() {
			close(in)
			if err := awaitAsync(t, done); err != nil {
				t.Error(err)
			}
			cancel()
		})
	}
	t.Cleanup(func() { cancel(); finish() })
	return finish
}

func TestConversationTurnBudgetDoesNotRenewAndFreshWakeRearms(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant("first", "second", "third", "fresh wake")
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	first := gate.awaitWindow(t, 1, true)
	if first.turns != 2 {
		t.Fatalf("initial budget = %d", first.turns)
	}
	injectBusy(in)
	awaitAsync(t, synth.spoken)
	second := gate.awaitWindow(t, 2, true)
	if second.turns != 1 || !second.until.Equal(first.until) {
		t.Fatalf("follow-up renewed authorization: first=%+v second=%+v", first, second)
	}
	injectBusy(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 3, false)
	injectBusy(in) // third wake-free input must not even reach STT
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("fresh wake reply = %q", got)
	}
	gate.awaitWindow(t, 4, true)
	finish()
	if got := planner.calls.Load(); got != 4 {
		t.Fatalf("planner calls = %d", got)
	}
	if got := a.STT.(*queuedTranscriber).next; got != 4 {
		t.Fatalf("ambient transcription calls = %d", got)
	}
}

func TestConversationAbsoluteDeadlineClosesBeforeAmbientTranscription(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant("first", "fresh wake")
	a.ConversationWindow = time.Millisecond
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 1, true)
	gate.awaitWindow(t, 1, false) // real expiry event, not a direct helper call
	injectBusy(in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("reply after expiry = %q", got)
	}
	finish()
	if planner.calls.Load() != 2 || a.STT.(*queuedTranscriber).next != 2 {
		t.Fatal("expired conversation reached STT or planner")
	}
}

func TestConversationExplicitCloseRearmsWithoutPlanner(t *testing.T) {
	for _, closePhrase := range []string{"that's all", "stop", "cancel"} {
		t.Run(closePhrase, func(t *testing.T) {
			a, in, gate, synth, planner := conversationAssistant("first", closePhrase, "fresh wake")
			finish := runConversation(t, a, in)
			injectWake(in)
			awaitAsync(t, synth.spoken)
			gate.awaitWindow(t, 1, true)
			injectBusy(in)
			awaitAsync(t, synth.spoken)
			injectBusy(in)
			injectWake(in)
			if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
				t.Fatalf("reply after close = %q", got)
			}
			finish()
			if planner.calls.Load() != 2 || a.STT.(*queuedTranscriber).next != 3 {
				t.Fatal("close or ambient input reached the planner")
			}
		})
	}
}

func TestConversationFailedAnswerDoesNotReopenCapture(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant("first", "second", "fresh wake")
	synth.failAt = 2
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 1, true)
	injectBusy(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 2, false)
	injectBusy(in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("reply after failure = %q", got)
	}
	finish()
	if planner.calls.Load() != 3 || a.STT.(*queuedTranscriber).next != 3 {
		t.Fatal("failed playback left ambient transcription authorized")
	}
}

type lateConversationTranscriber struct {
	calls   int
	started chan struct{}
	release chan struct{}
}

func (s *lateConversationTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	s.calls++
	if s.calls == 2 {
		close(s.started)
		<-s.release
	}
	return "request", nil
}
func (*lateConversationTranscriber) Name() string              { return "late transcriber" }
func (*lateConversationTranscriber) Available() (bool, string) { return true, "available" }

func TestConversationDeadlineCannotBeExtendedBySlowTranscription(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant()
	a.ConversationWindow = time.Millisecond
	stt := &lateConversationTranscriber{started: make(chan struct{}), release: make(chan struct{})}
	a.STT = stt
	release := sync.OnceFunc(func() { close(stt.release) })
	defer release()
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	window := gate.awaitWindow(t, 1, true)
	injectBusy(in)
	awaitAsync(t, stt.started)
	<-time.After(time.Until(window.until) + time.Millisecond)
	release()
	gate.awaitWindow(t, 1, false)
	finish()
	if planner.calls.Load() != 1 {
		t.Fatal("transcription returned after deadline but still started a backend action")
	}
}

func TestClosingConversationDuringWorkDoesNotReopenOnLateAnswer(t *testing.T) {
	a, in, gate, synth, _ := conversationAssistant("first", "that's all")
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "Finished."},
	}
	a.Brain = planner
	a.InFlightWindow, a.InFlightTurns = time.Minute, 2
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, planner.started)
	gate.awaitWindow(t, 0, true)
	injectBusy(in)
	if got := awaitAsync(t, synth.spoken); got != "Okay." {
		t.Fatalf("close reply = %q", got)
	}
	release()
	if got := awaitAsync(t, synth.spoken); got != "Finished." {
		t.Fatalf("final reply = %q", got)
	}
	gate.awaitWindow(t, 2, false)
	injectBusy(in)
	finish()
	if a.STT.(*queuedTranscriber).next != 2 {
		t.Fatal("late answer reopened an explicitly closed conversation")
	}
}

type conversationErrorTranscriber struct {
	queuedTranscriber
}

func (s *conversationErrorTranscriber) Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error) {
	if s.next == 1 {
		s.next++
		return "", errors.New("transcription unavailable")
	}
	return s.queuedTranscriber.Transcribe(ctx, pcm, f)
}

func TestConversationTranscriptionFailureClosesAuthorization(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant()
	stt := &conversationErrorTranscriber{queuedTranscriber{texts: []string{"first", "failed follow-up", "fresh wake"}}}
	a.STT = stt
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 1, true)
	injectBusy(in)
	gate.awaitWindow(t, 1, false)
	injectBusy(in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("rearmed reply = %q", got)
	}
	finish()
	if planner.calls.Load() != 2 || stt.next != 3 {
		t.Fatal("failed STT left wake-free authorization open")
	}
}

func TestSpokenShutdownWaitsForCaptureCleanup(t *testing.T) {
	a, in, _, synth, planner := conversationAssistant("turn off")
	recorder := &closingRecorder{cleaned: make(chan struct{}), slow: time.Millisecond}
	a.Recorder = recorder
	finish := runConversation(t, a, in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Turning off." {
		t.Fatalf("shutdown reply = %q", got)
	}
	finish()
	select {
	case <-recorder.cleaned:
	default:
		t.Fatal("spoken shutdown returned with microphone capture still held open")
	}
	if planner.calls.Load() != 0 {
		t.Fatal("shutdown reached the backend")
	}
}

type replayConversationGate struct {
	in   <-chan vad.Utterance
	gate *vad.Keyword
}

func (g *replayConversationGate) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	return g.in
}
func (g *replayConversationGate) SetInFlightWindow(until time.Time, turns int) {
	g.gate.SetInFlightWindow(until, turns)
}
func (g *replayConversationGate) Suspend()                       { g.gate.Suspend() }
func (g *replayConversationGate) ResumeAfter(tail time.Duration) { g.gate.ResumeAfter(tail) }
func (g *replayConversationGate) Accepts(u vad.Utterance) bool   { return g.gate.Accepts(u) }

func TestSessionDiscardsPrePlaybackQueuedAudioBeforeCloudSTT(t *testing.T) {
	in := make(chan vad.Utterance, 1)
	in <- vad.Utterance{WakeMatched: true, GateEpoch: 0, Format: audio.Default()}
	close(in)
	gate := &replayConversationGate{in: in, gate: vad.NewKeyword(vad.Params{}, nil, nil)}
	stt := &queuedTranscriber{texts: []string{"a stale command"}}
	planner := &conversationPlanner{}
	a := &Assistant{Recorder: testRecorder{}, Player: testPlayer{}, VAD: gate, STT: stt,
		TTS: testSynthesizer{}, Brain: planner, Devices: device.NewRegistry()}
	if err := a.Speak(context.Background(), "An answer."); err != nil {
		t.Fatal(err)
	}
	gate.ResumeAfter(0) // the old utterance must remain invalid after the tail ends
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stt.next != 0 || planner.calls.Load() != 0 {
		t.Fatal("queued pre-playback audio was transcribed or acted on")
	}
}

type conversationPlaybackFailure struct {
	testPlayer
	calls int
}

func (p *conversationPlaybackFailure) PlayWAV(context.Context, []byte) error {
	p.calls++
	if p.calls == 2 {
		return errors.New("speaker unavailable")
	}
	return nil
}

func TestConversationSpeakerFailureClosesAuthorization(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant("first", "second", "fresh wake")
	a.Player = &conversationPlaybackFailure{}
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 1, true)
	injectBusy(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 2, false)
	injectBusy(in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("reply after speaker failure = %q", got)
	}
	finish()
	if planner.calls.Load() != 3 || a.STT.(*queuedTranscriber).next != 3 {
		t.Fatal("speaker failure kept ambient transcription open")
	}
}

type orderlyManagedSegmenter struct {
	exited      chan struct{}
	closed      chan struct{}
	closedEarly bool
}

func (s *orderlyManagedSegmenter) Run(ctx context.Context, _ <-chan []int16, _ audio.Format) <-chan vad.Utterance {
	out := make(chan vad.Utterance, 1)
	out <- vad.Utterance{WakeMatched: true, Format: audio.Default()}
	go func() {
		<-ctx.Done()
		close(s.exited)
		close(out)
	}()
	return out
}
func (*orderlyManagedSegmenter) Start() error              { return nil }
func (*orderlyManagedSegmenter) Name() string              { return "managed segmenter" }
func (*orderlyManagedSegmenter) Available() (bool, string) { return true, "available" }
func (s *orderlyManagedSegmenter) Close() {
	select {
	case <-s.exited:
	default:
		s.closedEarly = true
	}
	close(s.closed)
}

func TestSpokenShutdownStopsSegmentationBeforeFreeingDetector(t *testing.T) {
	segmenter := &orderlyManagedSegmenter{exited: make(chan struct{}), closed: make(chan struct{})}
	a := &Assistant{Recorder: testRecorder{}, Player: testPlayer{}, VAD: segmenter,
		STT: &queuedTranscriber{texts: []string{"turn off"}}, TTS: testSynthesizer{}, Devices: device.NewRegistry()}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if segmenter.closedEarly {
		t.Fatal("native detector was freed while segmentation was running")
	}
	select {
	case <-segmenter.exited:
	default:
		t.Fatal("native detector freed before segmentation exited")
	}
	select {
	case <-segmenter.closed:
	default:
		t.Fatal("native detector was not released after segmentation exited")
	}
}

func TestBuildWiresBoundedConversationAndThinkingCaptureGuard(t *testing.T) {
	c := config.Default()
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.ConversationWindow = config.Duration(20 * time.Second)
	c.Wake.ConversationTurns = 2
	c.Feedback.Thinking = audio.ThinkingTick
	a := Build(c)
	if a.ConversationWindow != 20*time.Second || a.ConversationTurns != 2 {
		t.Fatal("configured post-answer authorization was not wired")
	}
	if a.Front != nil {
		t.Fatal("conversation mode enabled a front model")
	}
	if a.Feedback.BeforeThinking == nil || a.Feedback.AfterThinking == nil || a.Feedback.CanThink == nil {
		t.Fatal("thinking playback lacks the local capture guard")
	}
	if a.Feedback.ThinkingGap <= playbackTail {
		t.Fatal("thinking holds the capture gate over the entire quiet gap")
	}
}

func TestLocalControlsUseBothRawAndWakeNormalizedTranscripts(t *testing.T) {
	if _, stop, handled := localControl("Turn off."); !stop || !handled {
		t.Fatal("a punctuated wake-free shutdown reached the planner")
	}
	for _, command := range []string{"what's happening", "what s happening", "how s it going"} {
		if inFlightControl(command) != "status" {
			t.Fatalf("%q lost deterministic status routing", command)
		}
	}
	for _, command := range []string{"That's all.", "that s all"} {
		if !conversationClose(command) {
			t.Fatalf("%q did not close conversation", command)
		}
	}
}
