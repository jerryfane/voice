package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/vad"
)

type injectedSegmenter struct{ utterances <-chan vad.Utterance }

func (s injectedSegmenter) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	return s.utterances
}

func injectWake(out chan<- vad.Utterance) {
	out <- vad.Utterance{Format: audio.Default(), WakeMatched: true}
}

type waitingPlanner struct {
	started chan struct{}
	release chan struct{}
	ended   chan struct{}
	calls   atomic.Int32
	plan    brain.Plan
}

func (p *waitingPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	p.calls.Add(1)
	close(p.started)
	<-p.release // deliberately ignores cancellation, like a stuck backend can
	defer close(p.ended)
	return p.plan, nil
}
func (*waitingPlanner) Name() string              { return "waiting planner" }
func (*waitingPlanner) Available() (bool, string) { return true, "available" }

type signalingSynthesizer struct{ spoken chan string }

func (s *signalingSynthesizer) Synthesize(_ context.Context, text string) ([]byte, error) {
	s.spoken <- text
	return audio.EncodeWAV([]int16{1}, audio.Default()), nil
}
func (*signalingSynthesizer) Name() string              { return "signaling synthesizer" }
func (*signalingSynthesizer) Available() (bool, string) { return true, "available" }

func awaitAsync[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for asynchronous session event")
		var zero T
		return zero
	}
}

func TestSlowPlannerAnswersAuthorizedBusyTurnBeforeCompletion(t *testing.T) {
	utterances := make(chan vad.Utterance)
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "First request completed."},
	}
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	player := newLifecyclePlayer()
	synth := &signalingSynthesizer{spoken: make(chan string, 2)}
	a := &Assistant{
		Recorder: testRecorder{}, Player: player, VAD: injectedSegmenter{utterances},
		STT: &queuedTranscriber{texts: []string{"first request", "second request"}},
		TTS: synth, Brain: planner, Devices: device.NewRegistry(),
		Feedback:    &feedback.Notifier{Player: player, Working: []int16{1}, ThinkingDelay: time.Millisecond, Format: audio.Default()},
		WakePhrases: []string{"hey voice"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	awaitAsync(t, player.started) // thinking is playing when the second turn arrives
	injectWake(utterances)
	if got := awaitAsync(t, synth.spoken); got != "I'm still working on your previous request." {
		t.Fatalf("busy turn spoke %q", got)
	}
	if calls := planner.calls.Load(); calls != 1 {
		t.Fatalf("started %d planner jobs before the first completed", calls)
	}
	release()
	if got := awaitAsync(t, synth.spoken); got != "First request completed." {
		t.Fatalf("first completion spoke %q", got)
	}
	close(utterances)
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
	if player.overlapped() {
		t.Fatal("thinking playback overlapped an answer")
	}
}

type countedDevice struct{ applied atomic.Int32 }

func (*countedDevice) ID() string                { return "test light" }
func (*countedDevice) Kind() string              { return "light" }
func (*countedDevice) Capabilities() []device.Op { return []device.Op{device.OpOn} }
func (d *countedDevice) Apply(context.Context, device.Command) (device.State, error) {
	d.applied.Add(1)
	return device.State{"power": "on"}, nil
}
func (*countedDevice) Read(context.Context) (device.State, error) { return nil, nil }
func (*countedDevice) Describe() string                           { return "test light" }

func TestTimerFiresDuringPlannerAndShutdownDiscardsLateActions(t *testing.T) {
	a, _, _ := timerAssistant(t)
	utterances := make(chan vad.Utterance)
	a.VAD = injectedSegmenter{utterances}
	a.STT = &queuedTranscriber{texts: []string{"turn on the light"}}
	synth := &signalingSynthesizer{spoken: make(chan string, 2)}
	a.TTS = synth
	store := proposalStore(t)
	a.Proposals = store
	light := &countedDevice{}
	a.Devices.Add(light)
	proposal := brain.Proposal{Request: "change the light", Title: "Change the light"}
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "Done.", Actions: []brain.Action{{Device: light.ID(), Op: device.OpOn}}, Proposal: &proposal},
	}
	a.Brain = planner
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	if _, err := a.Timers.Add(20*time.Millisecond, "brief"); err != nil {
		t.Fatal(err)
	}
	if got := awaitAsync(t, synth.spoken); !strings.Contains(got, "timer is done") {
		t.Fatalf("timer announcement while planner waited = %q", got)
	}
	cancel()
	release() // the backend returns its plan despite cancellation
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
	awaitAsync(t, planner.ended)
	if light.applied.Load() != 0 {
		t.Fatal("a cancelled planner executed a late device action")
	}
	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("a cancelled planner filed %d late proposals", len(stored))
	}
	select {
	case got := <-synth.spoken:
		t.Fatalf("spoke late cancelled answer %q", got)
	default:
	}
}

func TestShutdownBoundsUncooperativePlannerJoin(t *testing.T) {
	utterances := make(chan vad.Utterance)
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	a := &Assistant{
		Recorder: testRecorder{}, Player: testPlayer{}, VAD: injectedSegmenter{utterances},
		STT: &queuedTranscriber{texts: []string{"slow request"}}, TTS: testSynthesizer{},
		Brain: planner, Devices: device.NewRegistry(), WakePhrases: []string{"hey voice"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	started := time.Now()
	cancel()
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > plannerStopTimeout+time.Second {
		t.Fatalf("shutdown waited %s for an uncooperative planner", elapsed)
	}
	release()
	awaitAsync(t, planner.ended)
}

type injectedConversationGate struct {
	in     <-chan vad.Utterance
	opened chan struct{}
}

func (g injectedConversationGate) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	return g.in
}
func (g injectedConversationGate) SetInFlightWindow(until time.Time, turns int) {
	if !until.IsZero() && turns > 0 {
		select {
		case g.opened <- struct{}{}:
		default:
		}
	}
}
func (injectedConversationGate) Suspend()                  {}
func (injectedConversationGate) ResumeAfter(time.Duration) {}

func injectBusy(out chan<- vad.Utterance) {
	out <- vad.Utterance{Format: audio.Default(), WakeMatched: true, WakeAcknowledged: true, Keyword: "IN_FLIGHT"}
}

func TestInFlightStatusAndCancelPrecedeSlowPlannerResult(t *testing.T) {
	utterances := make(chan vad.Utterance)
	gate := injectedConversationGate{in: utterances, opened: make(chan struct{}, 2)}
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "Late work completed."},
	}
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	synth := &signalingSynthesizer{spoken: make(chan string, 4)}
	a := &Assistant{
		Recorder: testRecorder{}, Player: testPlayer{}, VAD: gate,
		STT: &queuedTranscriber{texts: []string{"research that", "status", "cancel that"}},
		TTS: synth, Brain: planner, Devices: device.NewRegistry(),
		WakePhrases: []string{"hey voice"}, InFlightWindow: time.Minute, InFlightTurns: 2,
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	awaitAsync(t, gate.opened)
	injectBusy(utterances)
	if got := awaitAsync(t, synth.spoken); !strings.Contains(got, "don't have a confirmed result") {
		t.Fatalf("unverified status = %q", got)
	}
	awaitAsync(t, gate.opened)
	injectBusy(utterances)
	if got := awaitAsync(t, synth.spoken); !strings.Contains(got, "asked it to stop") {
		t.Fatalf("cancel acknowledgement = %q", got)
	}
	if planner.calls.Load() != 1 {
		t.Fatalf("in-flight turns spawned %d planner jobs, want one", planner.calls.Load())
	}
	release()
	if got := awaitAsync(t, synth.spoken); !strings.Contains(got, "request has ended") {
		t.Fatalf("confirmed cancellation = %q", got)
	}
	close(utterances)
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-synth.spoken:
		t.Fatalf("cancelled task spoke an extra late answer: %q", got)
	default:
	}
}

type testFrontDesk struct{ calls atomic.Int32 }

func (f *testFrontDesk) Reply(_ context.Context, utterance, status string) (string, error) {
	f.calls.Add(1)
	if utterance != "can you clarify" || status != "The previous request is still running." {
		return "", errors.New("front desk received wrong scoped context")
	}
	return "Did you mean the reminder for tomorrow?", nil
}

func TestFrontDeskSpeaksWhileBackendIsBusyWithoutSecondAgentTurn(t *testing.T) {
	utterances := make(chan vad.Utterance)
	gate := injectedConversationGate{in: utterances, opened: make(chan struct{}, 2)}
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "Done."},
	}
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	front := &testFrontDesk{}
	synth := &signalingSynthesizer{spoken: make(chan string, 4)}
	a := &Assistant{
		Recorder: testRecorder{}, Player: testPlayer{}, VAD: gate,
		STT: &queuedTranscriber{texts: []string{"find that", "can you clarify"}},
		TTS: synth, Brain: planner, Front: front, Devices: device.NewRegistry(),
		WakePhrases: []string{"hey voice"}, InFlightWindow: time.Minute, InFlightTurns: 1,
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	awaitAsync(t, gate.opened)
	injectBusy(utterances)
	if got := awaitAsync(t, synth.spoken); got != "Did you mean the reminder for tomorrow?" {
		t.Fatalf("front desk reply = %q", got)
	}
	if front.calls.Load() != 1 || planner.calls.Load() != 1 {
		t.Fatalf("front calls=%d agent calls=%d, want one each", front.calls.Load(), planner.calls.Load())
	}
	release()
	if got := awaitAsync(t, synth.spoken); got != "Done." {
		t.Fatalf("final answer = %q", got)
	}
	close(utterances)
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
}

type repeatingLifecyclePlayer struct {
	*lifecyclePlayer
	cycles chan struct{}
}

func (p *repeatingLifecyclePlayer) PlayPCM(ctx context.Context, _ []int16, _ audio.Format) error {
	p.mu.Lock()
	p.thinking = true
	p.mu.Unlock()
	select {
	case p.cycles <- struct{}{}:
	case <-ctx.Done():
	}
	<-ctx.Done()
	p.mu.Lock()
	p.thinking = false
	p.mu.Unlock()
	return ctx.Err()
}

func TestThinkingResumesDuringInFlightWindowAndAfterStatus(t *testing.T) {
	utterances := make(chan vad.Utterance)
	gate := injectedConversationGate{in: utterances, opened: make(chan struct{}, 2)}
	planner := &waitingPlanner{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		plan: brain.Plan{Speak: "Completed."},
	}
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	player := &repeatingLifecyclePlayer{lifecyclePlayer: newLifecyclePlayer(), cycles: make(chan struct{})}
	synth := &signalingSynthesizer{spoken: make(chan string, 4)}
	a := &Assistant{
		Recorder: testRecorder{}, Player: player, VAD: gate,
		STT: &queuedTranscriber{texts: []string{"slow request", "status"}},
		TTS: synth, Brain: planner, Devices: device.NewRegistry(),
		Feedback: &feedback.Notifier{
			Player: player, Working: []int16{1}, ThinkingDelay: time.Millisecond, Format: audio.Default(),
		},
		WakePhrases: []string{"hey voice"}, InFlightWindow: time.Minute, InFlightTurns: 2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	injectWake(utterances)
	awaitAsync(t, planner.started)
	awaitAsync(t, gate.opened)
	awaitAsync(t, player.cycles)
	select {
	case spoken := <-synth.spoken:
		t.Fatalf("unsolicited speech while work is pending: %q", spoken)
	default:
	}
	injectBusy(utterances)
	awaitAsync(t, synth.spoken)
	awaitAsync(t, gate.opened)
	awaitAsync(t, player.cycles)
	release()
	if got := awaitAsync(t, synth.spoken); got != planner.plan.Speak {
		t.Fatalf("final result = %q", got)
	}
	close(utterances)
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}
	if player.overlapped() {
		t.Fatal("thinking audio overlapped spoken feedback")
	}
}
